package store

// BlastSchema defines the tables Blast Radius adds on top of an existing
// Git Archaeologist index database.
//
// Design notes:
//
//   - We DO NOT recreate `symbols`, `edges`, `files`, etc. — they belong to
//     Git Archaeologist. We read them. If they're missing, we tell the user
//     to run `archaeo index` first instead of trying to create our own
//     half-broken graph.
//
//   - All blast tables are prefixed `blast_` so it's obvious which side owns
//     what. This lets multiple agents share a DB without stepping on each
//     other's namespace.
//
//   - `blast_impact_cache` memoises the result of an impact analysis so the
//     MCP server can answer repeat questions instantly. Cache key is the
//     symbol id + the analysis options hash. Invalidated on re-index by
//     wiping the table (we check `meta.last_index` and compare).
//
//   - `blast_test_map` links a test symbol (in *_test.go) to the production
//     symbols it transitively exercises. This is the expensive thing — we
//     compute it once after each index and reuse.
const BlastSchema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS blast_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Cache for impact-of-symbol queries.
-- key = sha256(symbol_id || options_hash). Wiped when archaeologist re-indexes.
CREATE TABLE IF NOT EXISTS blast_impact_cache (
    key         TEXT PRIMARY KEY,
    symbol_id   INTEGER NOT NULL,
    payload     TEXT NOT NULL,      -- JSON-encoded ImpactReport
    computed_at INTEGER NOT NULL    -- unix seconds
);
CREATE INDEX IF NOT EXISTS idx_blast_cache_sym ON blast_impact_cache(symbol_id);

-- Maps each test function to the production symbols it exercises (transitively).
-- Built once after each archaeologist index. Used by tests_to_run().
CREATE TABLE IF NOT EXISTS blast_test_map (
    test_symbol INTEGER NOT NULL,   -- references symbols.id
    prod_symbol INTEGER NOT NULL,   -- references symbols.id
    depth       INTEGER NOT NULL,   -- hop distance (1 = direct call)
    PRIMARY KEY (test_symbol, prod_symbol)
);
CREATE INDEX IF NOT EXISTS idx_blast_tm_prod ON blast_test_map(prod_symbol, depth);

-- Optional per-symbol pre-computed metrics. We persist them so MCP queries
-- stay snappy on big repos.
CREATE TABLE IF NOT EXISTS blast_metrics (
    symbol_id      INTEGER PRIMARY KEY,
    fan_in         INTEGER NOT NULL DEFAULT 0,  -- direct callers count
    fan_out        INTEGER NOT NULL DEFAULT 0,  -- direct callees count
    transitive_in  INTEGER NOT NULL DEFAULT 0,  -- total reachable callers
    is_exported    INTEGER NOT NULL DEFAULT 0,
    is_interface   INTEGER NOT NULL DEFAULT 0,
    risk_score     REAL NOT NULL DEFAULT 0      -- 0..100, computed
);
CREATE INDEX IF NOT EXISTS idx_blast_metrics_risk ON blast_metrics(risk_score DESC);
`
