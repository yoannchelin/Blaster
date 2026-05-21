// Package store_test exercises the basic store operations against a fresh
// in-memory archaeologist-shaped DB. This is the smoke test that catches
// schema/wiring regressions without requiring a real `archaeo index` run.
package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/yourname/blast-radius/internal/store"
)

// archaeoSchemaForTest is the minimum upstream schema we need to satisfy
// Store.Open's sanity check. We deliberately keep this in sync by hand
// with the real archaeologist schema — drift here is a feature, not a bug:
// if blast starts reading new columns, this test will fail and force an update.
const archaeoSchemaForTest = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE files (
    id INTEGER PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    package TEXT NOT NULL,
    loc INTEGER NOT NULL DEFAULT 0,
    is_test INTEGER NOT NULL DEFAULT 0,
    is_generated INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE symbols (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    qualified TEXT NOT NULL UNIQUE,
    file_id INTEGER,
    line_start INTEGER NOT NULL DEFAULT 0,
    line_end INTEGER NOT NULL DEFAULT 0,
    signature TEXT NOT NULL DEFAULT '',
    doc TEXT NOT NULL DEFAULT '',
    exported INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE edges (
    src INTEGER NOT NULL,
    dst INTEGER NOT NULL,
    relation TEXT NOT NULL,
    weight REAL NOT NULL DEFAULT 1.0,
    PRIMARY KEY (src, dst, relation)
);
CREATE INDEX idx_edges_dst ON edges(dst, relation);
CREATE INDEX idx_edges_src ON edges(src, relation);
CREATE TABLE commits (hash TEXT PRIMARY KEY, author TEXT, email TEXT, ts INTEGER, subject TEXT);
CREATE TABLE file_commits (file_id INTEGER, commit_hash TEXT, added INTEGER, deleted INTEGER, PRIMARY KEY(file_id, commit_hash));
`

// setupArchaeoLikeDB writes a minimal archaeologist DB layout into a temp dir
// and seeds it with three symbols: an interface, an implementer, a caller.
//
//	A (interface)     ← B (impl) ← C (calls B)
//
// This is enough to test impact and risk logic.
func setupArchaeoLikeDB(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".archaeo"), 0o755)
	dbPath := filepath.Join(dir, ".archaeo", "index.db")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(archaeoSchemaForTest); err != nil {
		t.Fatalf("schema: %v", err)
	}
	stmts := []string{
		`INSERT INTO files(path, package, loc) VALUES('a.go','example.com/x',50)`,
		`INSERT INTO symbols(kind,name,qualified,file_id,line_start,line_end,exported)
		 VALUES('interface','Provider','example.com/x.Provider',1,1,10,1),
		       ('method','Charge','example.com/x.Stripe.Charge',1,12,20,1),
		       ('func','HandleCharge','example.com/x.HandleCharge',1,22,30,1)`,
		// HandleCharge calls Stripe.Charge ; Stripe.Charge implements Provider.
		`INSERT INTO edges(src,dst,relation) VALUES(3,2,'calls'),(2,1,'implements')`,
		`INSERT INTO meta(key,value) VALUES('last_index','2026-05-01T00:00:00Z')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	return dir, func() { _ = os.RemoveAll(dir) }
}

func TestOpenAndLookup(t *testing.T) {
	dir, cleanup := setupArchaeoLikeDB(t)
	defer cleanup()

	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	sym, err := s.LookupSymbol("example.com/x.HandleCharge")
	if err != nil || sym == nil {
		t.Fatalf("lookup HandleCharge: %v %v", err, sym)
	}
	if sym.Kind != "func" {
		t.Errorf("kind = %q, want func", sym.Kind)
	}

	callers, err := s.IncomingCallers(2) // Stripe.Charge
	if err != nil {
		t.Fatalf("callers: %v", err)
	}
	if len(callers) != 1 || callers[0] != 3 {
		t.Errorf("expected HandleCharge as caller, got %v", callers)
	}

	impls, err := s.Implementations(1) // Provider
	if err != nil {
		t.Fatalf("impls: %v", err)
	}
	if len(impls) != 1 || impls[0] != 2 {
		t.Errorf("expected Stripe.Charge impl, got %v", impls)
	}
}

func TestOpenWithoutIndex(t *testing.T) {
	dir := t.TempDir()
	_, err := store.Open(dir)
	if err == nil {
		t.Fatal("expected error opening non-indexed repo")
	}
}
