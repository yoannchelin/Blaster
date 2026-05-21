// Package store wraps the SQLite database that Blast Radius reads and writes.
//
// We piggy-back on the database produced by Git Archaeologist
// (`<repo>/.archaeo/index.db`). On open, we:
//
//  1. verify that the archaeologist tables exist (symbols, edges, files…),
//  2. apply our `blast_*` tables idempotently,
//  3. compare archaeologist's `meta.last_index` to our cached value and
//     invalidate caches if it changed.
//
// We never write to archaeologist's tables. If you need to fix something
// upstream, run `archaeo index` again.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

// Store is a handle to a shared archaeo+blast SQLite database.
type Store struct {
	db   *sql.DB
	path string
}

// Symbol mirrors the archaeologist `symbols` row we care about.
type Symbol struct {
	ID        int64
	Kind      string
	Name      string
	Qualified string
	FileID    int64
	LineStart int
	LineEnd   int
	Signature string
	Doc       string
	Exported  bool
	Pagerank  float64
}

// File mirrors the archaeologist `files` row.
type File struct {
	ID          int64
	Path        string
	Package     string
	LOC         int
	IsTest      bool
	IsGenerated bool
}

// ErrNoIndex is returned when the archaeologist hasn't indexed this repo yet.
var ErrNoIndex = errors.New("no archaeologist index found — run `archaeo index` in this repo first")

// Open attaches to the index DB inside the given repo.
//
// repoRoot is the directory that contains `.archaeo/index.db`. We open the
// existing DB (read-write — we need to write `blast_*` tables) but never
// modify any archaeologist-owned table.
func Open(repoRoot string) (*Store, error) {
	dbPath := filepath.Join(repoRoot, ".archaeo", "index.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoIndex, dbPath)
	}
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Sanity check: do the upstream tables exist?
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='symbols'`,
	).Scan(&count); err != nil || count == 0 {
		_ = db.Close()
		return nil, fmt.Errorf("%w: symbols table missing in %s", ErrNoIndex, dbPath)
	}
	// Apply our schema.
	if _, err := db.Exec(BlastSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply blast schema: %w", err)
	}
	s := &Store{db: db, path: dbPath}
	// Invalidate caches if archaeologist re-indexed since our last write.
	if err := s.invalidateIfStale(); err != nil {
		// Non-fatal — caches are an optimisation, not correctness.
		_, _ = db.Exec(`DELETE FROM blast_impact_cache`)
	}
	return s, nil
}

// Close releases the DB handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw *sql.DB for analyses that need ad-hoc SQL.
func (s *Store) DB() *sql.DB { return s.db }

// Path is the on-disk path of the index file.
func (s *Store) Path() string { return s.path }

// --- Read helpers (archaeologist tables) -------------------------------------

// LookupSymbol finds a symbol by qualified name. Returns (nil, nil) if not found.
func (s *Store) LookupSymbol(qualified string) (*Symbol, error) {
	row := s.db.QueryRow(`
		SELECT id, kind, name, qualified, COALESCE(file_id, 0),
		       line_start, line_end, signature, doc, exported, pagerank
		FROM symbols WHERE qualified = ?`, qualified)
	return scanSymbol(row)
}

// GetSymbolByID fetches one symbol by id.
func (s *Store) GetSymbolByID(id int64) (*Symbol, error) {
	row := s.db.QueryRow(`
		SELECT id, kind, name, qualified, COALESCE(file_id, 0),
		       line_start, line_end, signature, doc, exported, pagerank
		FROM symbols WHERE id = ?`, id)
	return scanSymbol(row)
}

// GetFileByID fetches one file row.
func (s *Store) GetFileByID(id int64) (*File, error) {
	row := s.db.QueryRow(`
		SELECT id, path, package, loc, is_test, is_generated
		FROM files WHERE id = ?`, id)
	var f File
	var isTest, isGen int
	if err := row.Scan(&f.ID, &f.Path, &f.Package, &f.LOC, &isTest, &isGen); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	f.IsTest = isTest != 0
	f.IsGenerated = isGen != 0
	return &f, nil
}

// SymbolsInFile returns all top-level symbols defined in the given file path.
// Useful for "impact of a file" queries.
func (s *Store) SymbolsInFile(path string) ([]Symbol, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.kind, s.name, s.qualified, COALESCE(s.file_id, 0),
		       s.line_start, s.line_end, s.signature, s.doc, s.exported, s.pagerank
		FROM symbols s
		JOIN files f ON f.id = s.file_id
		WHERE f.path = ?
		  AND s.kind IN ('func','method','type','interface','var','const')`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		sym, err := scanSymbol(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sym)
	}
	return out, rows.Err()
}

// LookupFileByPath finds a file by its path relative to repo root.
func (s *Store) LookupFileByPath(path string) (*File, error) {
	row := s.db.QueryRow(`
		SELECT id, path, package, loc, is_test, is_generated
		FROM files WHERE path = ?`, path)
	var f File
	var isTest, isGen int
	if err := row.Scan(&f.ID, &f.Path, &f.Package, &f.LOC, &isTest, &isGen); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	f.IsTest = isTest != 0
	f.IsGenerated = isGen != 0
	return &f, nil
}

// IncomingCallers returns the direct callers of the given symbol id.
// This is the cornerstone of impact analysis.
func (s *Store) IncomingCallers(symbolID int64) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT src FROM edges WHERE dst = ? AND relation = 'calls'`,
		symbolID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Implementations returns the symbols that implement the given interface.
// Used to expand impact: changing an interface's signature potentially
// breaks every implementer.
func (s *Store) Implementations(interfaceID int64) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT src FROM edges WHERE dst = ? AND relation = 'implements'`,
		interfaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ChurnForFile returns total (added + deleted) lines across recorded history.
// Returns 0 if the file has no recorded commits.
func (s *Store) ChurnForFile(fileID int64) (int, int, error) {
	var churn, commits int
	err := s.db.QueryRow(`
		SELECT COALESCE(SUM(added + deleted), 0),
		       COUNT(DISTINCT commit_hash)
		FROM file_commits WHERE file_id = ?`, fileID).Scan(&churn, &commits)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	return churn, commits, err
}

// LastIndexedAt returns the upstream meta.last_index value (RFC3339 string),
// or "" if it's not set.
func (s *Store) LastIndexedAt() (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'last_index'`).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// --- Write helpers (blast_* tables) ------------------------------------------

// SetBlastMeta stores a key/value in our private meta table.
func (s *Store) SetBlastMeta(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO blast_meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// GetBlastMeta returns the value or ("", false).
func (s *Store) GetBlastMeta(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM blast_meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// PutMetric upserts a metrics row for a symbol.
func (s *Store) PutMetric(m Metrics) error {
	_, err := s.db.Exec(`
		INSERT INTO blast_metrics
		    (symbol_id, fan_in, fan_out, transitive_in, is_exported, is_interface, risk_score)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(symbol_id) DO UPDATE SET
		    fan_in = excluded.fan_in,
		    fan_out = excluded.fan_out,
		    transitive_in = excluded.transitive_in,
		    is_exported = excluded.is_exported,
		    is_interface = excluded.is_interface,
		    risk_score = excluded.risk_score`,
		m.SymbolID, m.FanIn, m.FanOut, m.TransitiveIn,
		boolToInt(m.IsExported), boolToInt(m.IsInterface), m.RiskScore)
	return err
}

// GetMetric returns the metrics row for a symbol, or nil if not present.
func (s *Store) GetMetric(symbolID int64) (*Metrics, error) {
	row := s.db.QueryRow(`
		SELECT symbol_id, fan_in, fan_out, transitive_in, is_exported, is_interface, risk_score
		FROM blast_metrics WHERE symbol_id = ?`, symbolID)
	var m Metrics
	var exp, iface int
	if err := row.Scan(&m.SymbolID, &m.FanIn, &m.FanOut, &m.TransitiveIn,
		&exp, &iface, &m.RiskScore); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	m.IsExported = exp != 0
	m.IsInterface = iface != 0
	return &m, nil
}

// PutTestMapping records that test_symbol exercises prod_symbol at the given depth.
func (s *Store) PutTestMapping(testSym, prodSym int64, depth int) error {
	_, err := s.db.Exec(`
		INSERT INTO blast_test_map(test_symbol, prod_symbol, depth)
		VALUES(?, ?, ?)
		ON CONFLICT(test_symbol, prod_symbol) DO UPDATE SET
		    depth = MIN(depth, excluded.depth)`,
		testSym, prodSym, depth)
	return err
}

// TestsExercising returns test symbol ids that exercise the given prod symbol,
// ordered by hop distance (closest first).
func (s *Store) TestsExercising(prodSymbolID int64) ([]TestHit, error) {
	rows, err := s.db.Query(`
		SELECT test_symbol, depth FROM blast_test_map
		WHERE prod_symbol = ? ORDER BY depth ASC`, prodSymbolID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TestHit
	for rows.Next() {
		var t TestHit
		if err := rows.Scan(&t.TestSymbolID, &t.Depth); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TestHit is one entry in TestsExercising's result.
type TestHit struct {
	TestSymbolID int64
	Depth        int
}

// Metrics is the persisted form of per-symbol blast metrics.
type Metrics struct {
	SymbolID     int64
	FanIn        int
	FanOut       int
	TransitiveIn int
	IsExported   bool
	IsInterface  bool
	RiskScore    float64
}

// CacheImpact stores an ImpactReport payload under a cache key.
func (s *Store) CacheImpact(key string, symbolID int64, payload string, ts int64) error {
	_, err := s.db.Exec(`
		INSERT INTO blast_impact_cache(key, symbol_id, payload, computed_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
		    payload = excluded.payload,
		    computed_at = excluded.computed_at`,
		key, symbolID, payload, ts)
	return err
}

// LoadImpactCache fetches a payload by key, or ("", false).
func (s *Store) LoadImpactCache(key string) (string, bool, error) {
	var p string
	err := s.db.QueryRow(
		`SELECT payload FROM blast_impact_cache WHERE key = ?`, key).Scan(&p)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return p, true, nil
}

// AllSymbolsForMetrics returns the symbols we want to compute metrics for.
// We skip vars/consts and generated files for noise reduction.
func (s *Store) AllSymbolsForMetrics() ([]Symbol, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.kind, s.name, s.qualified, COALESCE(s.file_id, 0),
		       s.line_start, s.line_end, s.signature, s.doc, s.exported, s.pagerank
		FROM symbols s
		LEFT JOIN files f ON f.id = s.file_id
		WHERE s.kind IN ('func','method','type','interface')
		  AND (f.is_generated IS NULL OR f.is_generated = 0)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		sym, err := scanSymbol(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sym)
	}
	return out, rows.Err()
}

// AllTestFunctions returns test functions (kind='func', file is_test=1, name starts with Test/Benchmark/Fuzz).
func (s *Store) AllTestFunctions() ([]Symbol, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.kind, s.name, s.qualified, COALESCE(s.file_id, 0),
		       s.line_start, s.line_end, s.signature, s.doc, s.exported, s.pagerank
		FROM symbols s
		JOIN files f ON f.id = s.file_id
		WHERE s.kind = 'func'
		  AND f.is_test = 1
		  AND (s.name LIKE 'Test%' OR s.name LIKE 'Benchmark%' OR s.name LIKE 'Fuzz%')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		sym, err := scanSymbol(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sym)
	}
	return out, rows.Err()
}

// FanInOutCounts returns direct caller and callee counts for a symbol.
func (s *Store) FanInOutCounts(symbolID int64) (in, out int, err error) {
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE dst = ? AND relation = 'calls'`,
		symbolID).Scan(&in)
	if err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE src = ? AND relation = 'calls'`,
		symbolID).Scan(&out)
	return in, out, err
}

// --- internal helpers --------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSymbol(r rowScanner) (*Symbol, error) {
	var s Symbol
	var exported int
	if err := r.Scan(&s.ID, &s.Kind, &s.Name, &s.Qualified, &s.FileID,
		&s.LineStart, &s.LineEnd, &s.Signature, &s.Doc, &exported, &s.Pagerank); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	s.Exported = exported != 0
	return &s, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// invalidateIfStale wipes our caches if the archaeologist re-indexed since
// the last time we wrote anything. We compare the upstream `meta.last_index`
// (set by archaeologist) to our stored copy.
func (s *Store) invalidateIfStale() error {
	upstream, err := s.LastIndexedAt()
	if err != nil || upstream == "" {
		return err
	}
	ours, _, err := s.GetBlastMeta("seen_index")
	if err != nil {
		return err
	}
	if ours == upstream {
		return nil
	}
	// Different — invalidate.
	if _, err := s.db.Exec(`DELETE FROM blast_impact_cache`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM blast_test_map`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM blast_metrics`); err != nil {
		return err
	}
	return s.SetBlastMeta("seen_index", upstream)
}
