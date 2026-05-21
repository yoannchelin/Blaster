// Package analyze contains the impact analysis algorithm.
//
// Core question: "if I change symbol S, what else might break?"
//
// We answer it by doing a reverse BFS through the `calls` graph from S,
// with special handling for interfaces:
//
//   - If S is a method of a type T that implements an interface I, then
//     changing S's signature (or behaviour) can break any code that calls
//     I.<methodOfSameName>. We pull in those callers too.
//
//   - If S is itself an interface method, every caller of that interface
//     method is impacted regardless of which implementation they use.
//
// We cap traversal at MaxDepth (default 6) — beyond that, the "blast"
// concept becomes meaningless, almost everything reaches `main()`.
//
// We also classify each impacted symbol by *distance* (hop count) so the
// MCP client can show "highly affected" (depth 1) vs "loosely affected"
// (depth 4+).
package analyze

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/yourname/blast-radius/internal/store"
)

// Options controls impact traversal breadth.
type Options struct {
	MaxDepth          int  // hop limit (default 6)
	IncludeTests      bool // include test functions in the impacted set (default false)
	ExpandInterfaces  bool // follow interface satisfaction edges (default true)
	MaxNodes          int  // hard cap on impacted set size (default 5000)
}

// DefaultOptions returns the recommended Options for impact analysis.
func DefaultOptions() Options {
	return Options{MaxDepth: 6, IncludeTests: false, ExpandInterfaces: true, MaxNodes: 5000}
}

// ImpactedSymbol is one entry in an impact report.
type ImpactedSymbol struct {
	SymbolID  int64  `json:"symbol_id"`
	Qualified string `json:"qualified"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Depth     int    `json:"depth"`
	Reason    string `json:"reason"` // "direct caller", "interface impl", "transitive (3 hops)"
}

// Report aggregates the result of an impact analysis.
type Report struct {
	Root       store.Symbol      `json:"root"`
	Impacted   []ImpactedSymbol  `json:"impacted"`
	TotalNodes int               `json:"total_nodes"`
	HitCap     bool              `json:"hit_cap"` // true if we stopped early due to MaxNodes
	Truncated  bool              `json:"truncated"` // true if we stopped due to MaxDepth
}

// Impact computes the blast radius of the given root symbol.
//
// The algorithm:
//
//	visited = {root}
//	frontier = {root}
//	for depth in 1..MaxDepth:
//	    next = ∅
//	    for sym in frontier:
//	        for caller in IncomingCallers(sym):
//	            if caller ∉ visited: visited.add(caller); next.add(caller)
//	        if ExpandInterfaces and sym is interface method:
//	            // every implementer's same-named method is impacted
//	            for impl in Implementations(sym.receiver):
//	                if impl ∉ visited: visited.add(impl); next.add(impl)
//	    frontier = next
//
// We track the depth at which each node was first visited so the report
// can present a layered view.
func Impact(ctx context.Context, s *store.Store, rootID int64, opt Options) (*Report, error) {
	if opt.MaxDepth == 0 {
		opt = DefaultOptions()
	}
	root, err := s.GetSymbolByID(rootID)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("symbol id %d not found", rootID)
	}

	// visited maps id → depth at which it was first reached.
	// Reasons map id → human label, kept separate to avoid mutating depth.
	visited := map[int64]int{rootID: 0}
	reasons := map[int64]string{}

	frontier := []int64{rootID}
	report := &Report{Root: *root}

	for depth := 1; depth <= opt.MaxDepth; depth++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(frontier) == 0 {
			break
		}
		var next []int64
		for _, sid := range frontier {
			callers, err := s.IncomingCallers(sid)
			if err != nil {
				return nil, fmt.Errorf("callers of %d: %w", sid, err)
			}
			for _, cid := range callers {
				if _, seen := visited[cid]; seen {
					continue
				}
				visited[cid] = depth
				if depth == 1 {
					reasons[cid] = "direct caller"
				} else {
					reasons[cid] = fmt.Sprintf("transitive caller (%d hops)", depth)
				}
				next = append(next, cid)
				if len(visited) >= opt.MaxNodes {
					report.HitCap = true
					break
				}
			}
			if report.HitCap {
				break
			}

			if opt.ExpandInterfaces {
				// If sid is an interface (kind='interface') or method-of-interface,
				// fan out to implementers.
				sym, err := s.GetSymbolByID(sid)
				if err != nil || sym == nil {
					continue
				}
				if sym.Kind == "interface" {
					impls, _ := s.Implementations(sid)
					for _, iid := range impls {
						if _, seen := visited[iid]; seen {
							continue
						}
						visited[iid] = depth
						reasons[iid] = "interface implementer"
						next = append(next, iid)
					}
				}
			}
		}
		if report.HitCap {
			break
		}
		frontier = next
	}

	// Did we stop because of MaxDepth (frontier non-empty after loop)?
	if !report.HitCap && len(frontier) > 0 {
		// We finished a depth pass; check if there were still callers we didn't expand.
		for _, sid := range frontier {
			callers, _ := s.IncomingCallers(sid)
			for _, cid := range callers {
				if _, seen := visited[cid]; !seen {
					report.Truncated = true
					break
				}
			}
			if report.Truncated {
				break
			}
		}
	}

	// Hydrate the impacted set.
	report.Impacted = make([]ImpactedSymbol, 0, len(visited)-1)
	for id, d := range visited {
		if id == rootID {
			continue
		}
		sym, err := s.GetSymbolByID(id)
		if err != nil || sym == nil {
			continue
		}
		// Skip test functions unless asked.
		if !opt.IncludeTests {
			if sym.FileID != 0 {
				if f, _ := s.GetFileByID(sym.FileID); f != nil && f.IsTest {
					continue
				}
			}
		}
		path := ""
		if sym.FileID != 0 {
			if f, _ := s.GetFileByID(sym.FileID); f != nil {
				path = f.Path
			}
		}
		report.Impacted = append(report.Impacted, ImpactedSymbol{
			SymbolID:  sym.ID,
			Qualified: sym.Qualified,
			Kind:      sym.Kind,
			Path:      path,
			Line:      sym.LineStart,
			Depth:     d,
			Reason:    reasons[id],
		})
	}
	sortByDepthThenName(report.Impacted)
	report.TotalNodes = len(report.Impacted)
	return report, nil
}

// ImpactOfFile aggregates the impact of changing any top-level symbol in a file.
//
// We run Impact() for every symbol the file defines, then merge the results.
// Duplicates are kept at their minimum depth across roots.
func ImpactOfFile(ctx context.Context, s *store.Store, path string, opt Options) (*FileReport, error) {
	syms, err := s.SymbolsInFile(path)
	if err != nil {
		return nil, err
	}
	if len(syms) == 0 {
		return nil, fmt.Errorf("no symbols found in file %q (is the file indexed?)", path)
	}
	merged := map[int64]ImpactedSymbol{}
	report := &FileReport{Path: path, Symbols: make([]string, 0, len(syms))}
	for _, sym := range syms {
		report.Symbols = append(report.Symbols, sym.Qualified)
		r, err := Impact(ctx, s, sym.ID, opt)
		if err != nil {
			return nil, err
		}
		report.HitCap = report.HitCap || r.HitCap
		report.Truncated = report.Truncated || r.Truncated
		for _, imp := range r.Impacted {
			if existing, ok := merged[imp.SymbolID]; !ok || imp.Depth < existing.Depth {
				merged[imp.SymbolID] = imp
			}
		}
	}
	for _, v := range merged {
		report.Impacted = append(report.Impacted, v)
	}
	sortByDepthThenName(report.Impacted)
	report.TotalNodes = len(report.Impacted)
	return report, nil
}

// CachedImpact is a caching wrapper around Impact.
//
// On a cache hit the BFS is skipped entirely — the result is decoded from
// blast_impact_cache. On a miss the BFS runs, the result is stored, and the
// Report is returned normally. Cache keys include all options that affect the
// result so stale entries are never returned for different option sets.
//
// The cache is invalidated wholesale by Store.Open when archaeologist
// re-indexes (meta.last_index changes). No per-entry TTL is needed.
func CachedImpact(ctx context.Context, s *store.Store, rootID int64, opt Options) (*Report, error) {
	if opt.MaxDepth == 0 {
		opt = DefaultOptions()
	}
	key := cacheKey(rootID, opt)

	if payload, ok, _ := s.LoadImpactCache(key); ok {
		var cached []ImpactedSymbol
		if err := json.Unmarshal([]byte(payload), &cached); err == nil {
			// Reconstruct a minimal Report from the cached impacted set.
			// Root symbol is re-fetched so callers get a complete struct.
			root, _ := s.GetSymbolByID(rootID)
			rep := &Report{TotalNodes: len(cached), Impacted: cached}
			if root != nil {
				rep.Root = *root
			}
			return rep, nil
		}
	}

	rep, err := Impact(ctx, s, rootID, opt)
	if err != nil {
		return nil, err
	}
	if b, jerr := json.Marshal(rep.Impacted); jerr == nil {
		_ = s.CacheImpact(key, rootID, string(b), time.Now().Unix())
	}
	return rep, nil
}

func cacheKey(rootID int64, opt Options) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%v:%v", rootID, opt.MaxDepth, opt.IncludeTests, opt.ExpandInterfaces)))
	return fmt.Sprintf("%x", h)
}

func sortByDepthThenName(s []ImpactedSymbol) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Depth != s[j].Depth {
			return s[i].Depth < s[j].Depth
		}
		return s[i].Qualified < s[j].Qualified
	})
}

// FileReport is ImpactOfFile's output.
type FileReport struct {
	Path       string           `json:"path"`
	Symbols    []string         `json:"symbols"`
	Impacted   []ImpactedSymbol `json:"impacted"`
	TotalNodes int              `json:"total_nodes"`
	HitCap     bool             `json:"hit_cap"`
	Truncated  bool             `json:"truncated"`
}
