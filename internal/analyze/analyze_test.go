package analyze_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/yourname/blast-radius/internal/analyze"
	"github.com/yourname/blast-radius/internal/store"
)

// We rebuild a tiny archaeologist DB inline rather than depending on the
// store_test fixture, because Go test packages can't import each other.
//
//	main → HandleCharge → Stripe.Charge (implements Provider)
//
// Changing Provider should impact: Stripe.Charge (impl), HandleCharge (caller of impl), main (caller of HandleCharge).
const seedSchema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE files (id INTEGER PRIMARY KEY, path TEXT NOT NULL UNIQUE, package TEXT NOT NULL, loc INTEGER, is_test INTEGER DEFAULT 0, is_generated INTEGER DEFAULT 0);
CREATE TABLE symbols (id INTEGER PRIMARY KEY, kind TEXT, name TEXT, qualified TEXT UNIQUE, file_id INTEGER, line_start INTEGER, line_end INTEGER, signature TEXT DEFAULT '', doc TEXT DEFAULT '', exported INTEGER DEFAULT 0);
CREATE TABLE edges (src INTEGER, dst INTEGER, relation TEXT, weight REAL DEFAULT 1.0, PRIMARY KEY(src,dst,relation));
CREATE TABLE commits (hash TEXT PRIMARY KEY, author TEXT, email TEXT, ts INTEGER, subject TEXT);
CREATE TABLE file_commits (file_id INTEGER, commit_hash TEXT, added INTEGER, deleted INTEGER, PRIMARY KEY(file_id, commit_hash));
`

func setupDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".archaeo"), 0o755)
	db, err := sql.Open("sqlite3", filepath.Join(dir, ".archaeo", "index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(seedSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	stmts := []string{
		`INSERT INTO files VALUES(1,'a.go','example.com/x',50,0,0)`,
		`INSERT INTO symbols(id,kind,name,qualified,file_id,line_start,line_end,exported)
		 VALUES(1,'interface','Provider','example.com/x.Provider',1,1,10,1),
		       (2,'method','Charge','example.com/x.Stripe.Charge',1,12,20,1),
		       (3,'func','HandleCharge','example.com/x.HandleCharge',1,22,30,1),
		       (4,'func','main','example.com/x.main',1,32,40,0)`,
		`INSERT INTO edges(src,dst,relation) VALUES
		    (2,1,'implements'),
		    (3,2,'calls'),
		    (4,3,'calls')`,
		`INSERT INTO meta(key,value) VALUES('last_index','2026-05-01T00:00:00Z')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	return dir
}

// TestImpactOfInterface verifies that changing an interface propagates to
// its implementers and to their callers.
func TestImpactOfInterface(t *testing.T) {
	dir := setupDB(t)
	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer s.Close()

	opt := analyze.DefaultOptions()
	rep, err := analyze.Impact(context.Background(), s, 1 /* Provider */, opt)
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	if rep.TotalNodes < 1 {
		t.Fatalf("expected at least 1 impacted symbol, got %d", rep.TotalNodes)
	}
	// Must contain Stripe.Charge as interface impl.
	found := map[string]bool{}
	for _, i := range rep.Impacted {
		found[i.Qualified] = true
	}
	if !found["example.com/x.Stripe.Charge"] {
		t.Errorf("expected Stripe.Charge in impact set, got: %v", found)
	}
}

// TestImpactDepth verifies the depth field tracks BFS distance correctly.
func TestImpactDepth(t *testing.T) {
	dir := setupDB(t)
	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer s.Close()

	// From Stripe.Charge: depth 1 = HandleCharge, depth 2 = main.
	rep, err := analyze.Impact(context.Background(), s, 2, analyze.DefaultOptions())
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	depth := map[string]int{}
	for _, i := range rep.Impacted {
		depth[i.Qualified] = i.Depth
	}
	if depth["example.com/x.HandleCharge"] != 1 {
		t.Errorf("HandleCharge depth = %d, want 1", depth["example.com/x.HandleCharge"])
	}
	if depth["example.com/x.main"] != 2 {
		t.Errorf("main depth = %d, want 2", depth["example.com/x.main"])
	}
}
