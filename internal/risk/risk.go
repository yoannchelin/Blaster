// Package risk computes a 0..100 risk score per symbol.
//
// The score combines five factors, each normalised to 0..1 and weighted.
// The weights are tuned so that the typical "core utility called from
// everywhere" lands around 80-90, a "private helper" lands around 10-20,
// and an unused symbol lands at 0.
//
// Factors and weights:
//
//	fan_in         (0.10) — direct callers (linear saturation at 50). Immediate blast surface.
//	transitive_in  (0.15) — transitive callers, sqrt-saturated at 200: anything above is "core".
//	is_exported    (0.15) — exported symbols are part of a public surface.
//	is_interface   (0.15) — interfaces affect every implementer.
//	churn          (0.20) — recent edits → unstable area, higher risk to touch.
//	loc_in_file    (0.15) — symbols in big files are usually load-bearing.
//	pagerank       (0.10) — graph centrality from archaeologist; 0 if not computed.
//
// We compute factors once after each archaeologist re-index and persist
// them in `blast_metrics`. The MCP server reads from there.
//
// The score is *advisory*, not prescriptive. The goal is to rank, not to
// gate: the user still decides whether to ship.
package risk

import (
	"context"
	"math"

	"github.com/yourname/blast-radius/internal/store"
)

// Weights for the risk formula. Exposed so they can be tuned without a recompile.
type Weights struct {
	FanIn        float64 // direct callers (linear saturation at 50)
	TransitiveIn float64 // transitive callers (sqrt saturation at 200)
	Exported     float64
	Interface    float64
	Churn        float64
	LOC          float64
	Pagerank     float64 // graph centrality from archaeologist; 0 if not computed
}

// DefaultWeights returns the tuned defaults. All seven weights sum to 1.0.
// FanIn captures the immediate blast surface (linear); TransitiveIn captures
// deep reachability (sqrt-saturated). Together they give better discrimination
// between a utility called from 5 places vs one called from 50+.
func DefaultWeights() Weights {
	return Weights{
		FanIn:        0.10,
		TransitiveIn: 0.15,
		Exported:     0.15,
		Interface:    0.15,
		Churn:        0.20,
		LOC:          0.15,
		Pagerank:     0.10,
	}
}

// Compute walks every interesting symbol, computes its risk components,
// and writes a `blast_metrics` row.
//
// progress is called every N symbols if non-nil.
//
// We compute transitive_in by reverse BFS up to depth 6 — same cap as
// the impact analysis. Beyond that the count is meaningless (everything
// reaches main).
func Compute(ctx context.Context, s *store.Store, w Weights, progress func(done, total int)) error {
	syms, err := s.AllSymbolsForMetrics()
	if err != nil {
		return err
	}
	total := len(syms)

	// Pre-fetch churn for every file once, to avoid one query per symbol.
	churnByFile, maxChurn, err := fetchFileChurn(s)
	if err != nil {
		return err
	}
	locByFile, maxLOC, err := fetchFileLOC(s)
	if err != nil {
		return err
	}

	for i, sym := range syms {
		if err := ctx.Err(); err != nil {
			return err
		}
		fanIn, fanOut, err := s.FanInOutCounts(sym.ID)
		if err != nil {
			return err
		}
		transIn, err := transitiveCallers(s, sym.ID, 6)
		if err != nil {
			return err
		}
		isIface := sym.Kind == "interface"

		// Normalise factors to 0..1.
		nFanIn := saturateLinear(float64(fanIn), 50)   // linear: direct callers saturate at 50
		nTrans := saturate(float64(transIn), 200)       // sqrt: deep reachability
		nExported := 0.0
		if sym.Exported {
			nExported = 1.0
		}
		nIface := 0.0
		if isIface {
			nIface = 1.0
		}
		nChurn := 0.0
		if maxChurn > 0 && sym.FileID != 0 {
			nChurn = float64(churnByFile[sym.FileID]) / float64(maxChurn)
		}
		nLOC := 0.0
		if maxLOC > 0 && sym.FileID != 0 {
			nLOC = float64(locByFile[sym.FileID]) / float64(maxLOC)
		}
		// Pagerank is already normalised to [0,1] by archaeologist (max node = 1.0).
		nPagerank := sym.Pagerank

		score := 100 * (
			w.FanIn*nFanIn +
				w.TransitiveIn*nTrans +
				w.Exported*nExported +
				w.Interface*nIface +
				w.Churn*nChurn +
				w.LOC*nLOC +
				w.Pagerank*nPagerank)

		if err := s.PutMetric(store.Metrics{
			SymbolID:     sym.ID,
			FanIn:        fanIn,
			FanOut:       fanOut,
			TransitiveIn: transIn,
			IsExported:   sym.Exported,
			IsInterface:  isIface,
			RiskScore:    score,
		}); err != nil {
			return err
		}
		if progress != nil && (i%50 == 0 || i == total-1) {
			progress(i+1, total)
		}
	}
	return nil
}

// transitiveCallers returns the count of distinct symbols that can reach
// the given symbol via `calls` edges, up to maxDepth hops.
//
// We deliberately cap depth instead of running an unbounded BFS: the
// counts converge quickly (typically by depth 4) and unbounded BFS on a
// large repo's main() is meaningless ("everything").
func transitiveCallers(s *store.Store, id int64, maxDepth int) (int, error) {
	visited := map[int64]bool{id: true}
	frontier := []int64{id}
	for d := 0; d < maxDepth; d++ {
		if len(frontier) == 0 {
			break
		}
		var next []int64
		for _, sid := range frontier {
			callers, err := s.IncomingCallers(sid)
			if err != nil {
				return 0, err
			}
			for _, cid := range callers {
				if !visited[cid] {
					visited[cid] = true
					next = append(next, cid)
				}
			}
		}
		frontier = next
	}
	return len(visited) - 1, nil
}

// fetchFileChurn returns a map of file_id → total churn, and the max value
// (used for normalisation).
func fetchFileChurn(s *store.Store) (map[int64]int, int, error) {
	rows, err := s.DB().Query(`
		SELECT file_id, COALESCE(SUM(added + deleted), 0)
		FROM file_commits GROUP BY file_id`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[int64]int{}
	maxV := 0
	for rows.Next() {
		var fid, c int64
		if err := rows.Scan(&fid, &c); err != nil {
			return nil, 0, err
		}
		out[fid] = int(c)
		if int(c) > maxV {
			maxV = int(c)
		}
	}
	return out, maxV, rows.Err()
}

// fetchFileLOC returns file_id → loc, with the max.
func fetchFileLOC(s *store.Store) (map[int64]int, int, error) {
	rows, err := s.DB().Query(`SELECT id, loc FROM files`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[int64]int{}
	maxV := 0
	for rows.Next() {
		var fid int64
		var loc int
		if err := rows.Scan(&fid, &loc); err != nil {
			return nil, 0, err
		}
		out[fid] = loc
		if loc > maxV {
			maxV = loc
		}
	}
	return out, maxV, rows.Err()
}

// saturate normalises x against ceiling with a sqrt curve: differences in the
// middle of the range are more visible than at the extremes.
func saturate(x, ceiling float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= ceiling {
		return 1
	}
	return math.Sqrt(x / ceiling)
}

// saturateLinear normalises x against ceiling linearly: each additional caller
// up to the ceiling contributes equally. Better for small counts (fan-in)
// where the difference between 5 and 10 direct callers is meaningful.
func saturateLinear(x, ceiling float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= ceiling {
		return 1
	}
	return x / ceiling
}
