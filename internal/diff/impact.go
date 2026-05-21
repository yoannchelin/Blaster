package diff

import (
	"context"
	"fmt"
	"sort"

	"github.com/yourname/blast-radius/internal/analyze"
	"github.com/yourname/blast-radius/internal/store"
)

// DiffImpactReport is the combined output of parsing a diff and computing
// impact for every symbol the diff touches.
type DiffImpactReport struct {
	Files          []FileImpact            `json:"files"`
	UniqueImpacted []analyze.ImpactedSymbol `json:"unique_impacted"`
	TotalFiles     int                     `json:"total_files"`
	TotalSymbols   int                     `json:"touched_symbols"`
	TotalImpacted  int                     `json:"unique_impacted_count"`
}

// FileImpact is one file's slice of the report.
type FileImpact struct {
	Path           string                   `json:"path"`
	OldPath        string                   `json:"old_path,omitempty"`
	IsNew          bool                     `json:"is_new"`
	IsRename       bool                     `json:"is_rename,omitempty"`
	TouchedSymbols []TouchedSymbol          `json:"touched_symbols"`
	Impacted       []analyze.ImpactedSymbol `json:"impacted"`
}

// TouchedSymbol is a symbol whose line range overlaps a diff hunk.
type TouchedSymbol struct {
	Qualified string `json:"qualified"`
	Kind      string `json:"kind"`
	Line      int    `json:"line"`
	HunkStart int    `json:"hunk_start"`
	HunkEnd   int    `json:"hunk_end"`
}

// AnalyzeFiles maps each TouchedFile to symbols and computes their impact.
//
// For each file:
//  1. Look up the file in the index.
//  2. Pull all symbols defined in the file.
//  3. For each hunk, find symbols whose [LineStart..LineEnd] overlaps
//     [Hunk.Start..Hunk.End].
//  4. Run analyze.Impact on each touched symbol.
//  5. Deduplicate across the whole diff so the user sees each impacted
//     symbol once, with its minimum depth.
//
// New files have no callers yet so their impact is trivially empty — we
// still report them so the user can see what's being added.
func AnalyzeFiles(
	ctx context.Context,
	s *store.Store,
	touched []TouchedFile,
	opt analyze.Options,
) (*DiffImpactReport, error) {
	report := &DiffImpactReport{TotalFiles: len(touched)}
	merged := map[int64]analyze.ImpactedSymbol{}

	for _, f := range touched {
		fi := FileImpact{Path: f.Path, IsNew: f.IsNew, IsRename: f.IsRename, OldPath: f.OldPath}

		// For renames, look up callers of the old path's symbols first.
		// The old symbols still exist in the index under their old qualified
		// names, and their callers are exactly who breaks if the rename
		// changed the package path or exported name.
		if f.IsRename && f.OldPath != "" {
			oldSyms, _ := s.SymbolsInFile(f.OldPath)
			for _, sym := range oldSyms {
				fi.TouchedSymbols = append(fi.TouchedSymbols, TouchedSymbol{
					Qualified: sym.Qualified,
					Kind:      sym.Kind,
					Line:      sym.LineStart,
					HunkStart: 1,
					HunkEnd:   sym.LineEnd,
				})
				rep, err := analyze.Impact(ctx, s, sym.ID, opt)
				if err != nil {
					return nil, fmt.Errorf("impact rename-old %s: %w", sym.Qualified, err)
				}
				for _, imp := range rep.Impacted {
					if existing, ok := merged[imp.SymbolID]; !ok || imp.Depth < existing.Depth {
						merged[imp.SymbolID] = imp
					}
				}
				fi.Impacted = append(fi.Impacted, rep.Impacted...)
			}
		}

		file, err := s.LookupFileByPath(f.Path)
		if err != nil {
			return nil, fmt.Errorf("lookup file %s: %w", f.Path, err)
		}
		if file == nil {
			report.Files = append(report.Files, fi)
			continue
		}

		syms, err := s.SymbolsInFile(f.Path)
		if err != nil {
			return nil, fmt.Errorf("symbols in %s: %w", f.Path, err)
		}

		// Intersect symbols with hunks.
		for _, sym := range syms {
			for _, h := range f.Hunks {
				if overlaps(sym.LineStart, sym.LineEnd, h.Start, h.End()) {
					fi.TouchedSymbols = append(fi.TouchedSymbols, TouchedSymbol{
						Qualified: sym.Qualified,
						Kind:      sym.Kind,
						Line:      sym.LineStart,
						HunkStart: h.Start,
						HunkEnd:   h.End(),
					})
					// Run impact for this symbol.
					rep, err := analyze.Impact(ctx, s, sym.ID, opt)
					if err != nil {
						return nil, fmt.Errorf("impact %s: %w", sym.Qualified, err)
					}
					for _, imp := range rep.Impacted {
						if existing, ok := merged[imp.SymbolID]; !ok || imp.Depth < existing.Depth {
							merged[imp.SymbolID] = imp
						}
					}
					fi.Impacted = append(fi.Impacted, rep.Impacted...)
					break // one hunk match is enough for this symbol
				}
			}
		}
		report.TotalSymbols += len(fi.TouchedSymbols)
		report.Files = append(report.Files, fi)
	}

	for _, v := range merged {
		report.UniqueImpacted = append(report.UniqueImpacted, v)
	}
	analyzeSort(report.UniqueImpacted)
	report.TotalImpacted = len(report.UniqueImpacted)
	return report, nil
}

func analyzeSort(s []analyze.ImpactedSymbol) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Depth != s[j].Depth {
			return s[i].Depth < s[j].Depth
		}
		return s[i].Qualified < s[j].Qualified
	})
}

// overlaps reports whether [a1,a2] and [b1,b2] (inclusive) intersect.
func overlaps(a1, a2, b1, b2 int) bool {
	if a2 < a1 {
		a2 = a1
	}
	if b2 < b1 {
		b2 = b1
	}
	return a1 <= b2 && b1 <= a2
}
