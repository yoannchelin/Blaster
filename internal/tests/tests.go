// Package tests builds the test→production-symbol map used by
// `tests_to_run` to answer "what should I retest after this change?".
//
// For every test function (kind='func' in a *_test.go file with name
// matching Test*/Benchmark*/Fuzz*), we walk the `calls` graph forward and
// record every prod symbol reachable from it, along with the hop distance.
//
// The result is persisted to `blast_test_map`. Lookup at MCP time is then
// a single indexed query, so "what tests touch payment.ChargeCustomer?"
// resolves in milliseconds.
//
// We cap BFS depth (default 6) because beyond that we're following
// shared helpers (logging, fmt) that almost every test reaches — useless.
package tests

import (
	"context"
	"fmt"
	"sort"

	"github.com/yourname/blast-radius/internal/store"
)

// Options controls the build.
type Options struct {
	MaxDepth int // hop cap (default 6)
}

// DefaultOptions returns recommended Options.
func DefaultOptions() Options {
	return Options{MaxDepth: 6}
}

// BuildMap walks the call graph from each test function and records every
// reachable production symbol in `blast_test_map`.
//
// We do a forward BFS from each test. Production symbols are anything not
// in a *_test.go file. We record the minimum depth at which we reached
// each prod symbol from each test, so the report can show "direct call"
// vs "called via 3 hops".
//
// progress reports per-test progress (test_index/total).
func BuildMap(ctx context.Context, s *store.Store, opt Options, progress func(done, total int)) (int, error) {
	if opt.MaxDepth == 0 {
		opt = DefaultOptions()
	}

	testFns, err := s.AllTestFunctions()
	if err != nil {
		return 0, err
	}
	total := len(testFns)
	written := 0

	// We need fast "is symbol id in a test file?" — cache the file is_test flag.
	testFileIDs, err := fetchTestFileIDs(s)
	if err != nil {
		return 0, err
	}

	for i, tf := range testFns {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		visited := map[int64]int{tf.ID: 0}
		frontier := []int64{tf.ID}
		for d := 1; d <= opt.MaxDepth; d++ {
			if len(frontier) == 0 {
				break
			}
			var next []int64
			for _, sid := range frontier {
				callees, err := outgoingCalls(s, sid)
				if err != nil {
					return written, fmt.Errorf("callees of %d: %w", sid, err)
				}
				for _, cid := range callees {
					if _, seen := visited[cid]; seen {
						continue
					}
					visited[cid] = d
					next = append(next, cid)
				}
			}
			frontier = next
		}
		// Record every visited symbol that lives in a non-test file.
		for sid, depth := range visited {
			if sid == tf.ID {
				continue
			}
			sym, err := s.GetSymbolByID(sid)
			if err != nil || sym == nil {
				continue
			}
			if sym.FileID == 0 || testFileIDs[sym.FileID] {
				continue
			}
			if err := s.PutTestMapping(tf.ID, sid, depth); err != nil {
				return written, fmt.Errorf("put mapping: %w", err)
			}
			written++
		}
		if progress != nil && (i%10 == 0 || i == total-1) {
			progress(i+1, total)
		}
	}
	return written, nil
}

// TestsForSymbols returns the union of tests that exercise any of the
// given prod symbols, ordered by best hop-distance first.
//
// This is what `tests_to_run` calls after computing impact. We deduplicate
// by test_symbol so the user doesn't see "TestFoo (depth 1, depth 3, depth 5)"
// — just "TestFoo (depth 1)".
func TestsForSymbols(s *store.Store, prodIDs []int64) ([]TestRecommendation, error) {
	best := map[int64]int{} // test id → minimum depth
	for _, pid := range prodIDs {
		hits, err := s.TestsExercising(pid)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			if d, ok := best[h.TestSymbolID]; !ok || h.Depth < d {
				best[h.TestSymbolID] = h.Depth
			}
		}
	}
	out := make([]TestRecommendation, 0, len(best))
	for tid, d := range best {
		sym, err := s.GetSymbolByID(tid)
		if err != nil || sym == nil {
			continue
		}
		path := ""
		if sym.FileID != 0 {
			if f, _ := s.GetFileByID(sym.FileID); f != nil {
				path = f.Path
			}
		}
		out = append(out, TestRecommendation{
			TestQualified: sym.Qualified,
			TestName:      sym.Name,
			Path:          path,
			Line:          sym.LineStart,
			MinDepth:      d,
		})
	}
	sortRecs(out)
	return out, nil
}

// TestRecommendation is one entry in the "tests to run" output.
type TestRecommendation struct {
	TestQualified string `json:"test_qualified"`
	TestName      string `json:"test_name"`
	Path          string `json:"path"`
	Line          int    `json:"line"`
	MinDepth      int    `json:"min_depth"`
}

func sortRecs(s []TestRecommendation) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].MinDepth != s[j].MinDepth {
			return s[i].MinDepth < s[j].MinDepth
		}
		return s[i].TestQualified < s[j].TestQualified
	})
}

// --- helpers -----------------------------------------------------------------

// fetchTestFileIDs returns a set of file ids where is_test=1.
// Used as a hot-path check during BFS to filter out test→test edges.
func fetchTestFileIDs(s *store.Store) (map[int64]bool, error) {
	rows, err := s.DB().Query(`SELECT id FROM files WHERE is_test = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// outgoingCalls returns the `calls` neighbours in the outgoing direction.
// Inlined here (instead of going through store.Neighbors) so we get a
// single-relation, single-hop query that the SQLite planner can serve
// purely from the `idx_edges_src` index.
func outgoingCalls(s *store.Store, id int64) ([]int64, error) {
	rows, err := s.DB().Query(
		`SELECT DISTINCT dst FROM edges WHERE src = ? AND relation = 'calls'`,
		id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
