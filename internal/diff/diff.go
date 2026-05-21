// Package diff parses a unified-format diff (the output of `git diff` or a
// patch file) and maps it back to symbols in the archaeologist index.
//
// We implement just enough of the unified diff format to extract:
//   - the new file path (`+++ b/path/to/file.go`)
//   - hunk headers and the new-line ranges they cover (`@@ -a,b +c,d @@`)
//
// Then we cross-reference those ranges against `symbols.line_start..line_end`
// to find which functions/methods/types the diff touches. That set is the
// input to the impact analysis.
//
// We deliberately don't try to do AST-level diff understanding — that's a
// rabbit hole. Line ranges are good enough to identify touched symbols.
// False positives (a symbol whose line range overlaps a comment-only change)
// are acceptable; they just cause us to over-estimate impact, which is safer
// than under-estimating it.
package diff

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// TouchedFile is one entry in the parsed diff.
type TouchedFile struct {
	Path    string      // path on the "+" (new) side, relative to repo root
	OldPath string      // non-empty only for renames; the pre-rename path
	Hunks   []HunkRange // line ranges affected on the new side
	IsNew   bool        // true if the file was created in this diff
	IsDel   bool        // true if the file was deleted
	IsRename bool       // true if the file was renamed (OldPath is set)
}

// HunkRange is one new-side line range from a `@@` header.
type HunkRange struct {
	Start int // 1-based first line touched
	Count int // number of lines (always >= 1)
}

// End returns the inclusive last line of the hunk.
func (h HunkRange) End() int {
	if h.Count <= 0 {
		return h.Start
	}
	return h.Start + h.Count - 1
}

// Parse reads a unified diff from r and returns one TouchedFile per file
// header it encountered. Unknown lines are tolerated and skipped — we only
// care about file headers and hunk headers.
func Parse(r io.Reader) ([]TouchedFile, error) {
	scanner := bufio.NewScanner(r)
	// Some diffs are big. Allow up to ~1MB per line, which covers any sane
	// diff hunk header or filename.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var files []TouchedFile
	var cur *TouchedFile

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "diff --git"):
			// Start of a new file block. Flush the current one.
			if cur != nil {
				files = append(files, *cur)
			}
			cur = &TouchedFile{}
		case strings.HasPrefix(line, "new file mode"):
			if cur != nil {
				cur.IsNew = true
			}
		case strings.HasPrefix(line, "deleted file mode"):
			if cur != nil {
				cur.IsDel = true
			}
		case strings.HasPrefix(line, "rename from "):
			// "rename from old/path.go" — git's explicit rename marker.
			if cur != nil {
				cur.OldPath = strings.TrimPrefix(line, "rename from ")
				cur.IsRename = true
			}
		case strings.HasPrefix(line, "rename to "):
			// "rename to new/path.go" — sets the canonical new path.
			if cur != nil {
				cur.Path = strings.TrimPrefix(line, "rename to ")
			}
		case strings.HasPrefix(line, "+++ "):
			// "+++ b/path/to/file.go" or "+++ /dev/null"
			// For renames, "rename to" already set the path; don't overwrite.
			if cur == nil {
				cur = &TouchedFile{}
			}
			if cur.Path == "" {
				cur.Path = stripBPrefix(strings.TrimPrefix(line, "+++ "))
			}
		case strings.HasPrefix(line, "@@"):
			if cur == nil {
				continue
			}
			hr, ok := parseHunkHeader(line)
			if ok {
				cur.Hunks = append(cur.Hunks, hr)
			}
		}
	}
	if cur != nil {
		files = append(files, *cur)
	}
	if err := scanner.Err(); err != nil {
		return files, fmt.Errorf("scan diff: %w", err)
	}
	// Drop deleted files — we have nothing to map them against anyway.
	out := files[:0]
	for _, f := range files {
		if f.IsDel || f.Path == "/dev/null" || f.Path == "" {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// parseHunkHeader extracts the new-side range from a "@@ -a,b +c,d @@" line.
//
// Format quick-reference:
//
//	@@ -<old_start>,<old_count> +<new_start>,<new_count> @@ optional context
//
// Counts may be omitted, defaulting to 1.
func parseHunkHeader(s string) (HunkRange, bool) {
	// Find "+" segment.
	plus := strings.Index(s, "+")
	if plus == -1 {
		return HunkRange{}, false
	}
	rest := s[plus+1:]
	// Cut at the next space.
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	// rest is now "c,d" or "c".
	var startStr, countStr string
	if comma := strings.IndexByte(rest, ','); comma >= 0 {
		startStr = rest[:comma]
		countStr = rest[comma+1:]
	} else {
		startStr = rest
		countStr = "1"
	}
	start, err := strconv.Atoi(startStr)
	if err != nil {
		return HunkRange{}, false
	}
	count, err := strconv.Atoi(countStr)
	if err != nil {
		count = 1
	}
	if count == 0 {
		// "+0,0" means the change is at the end-of-file insertion point.
		// We model this as a 1-line range at start so it still maps to a symbol.
		count = 1
	}
	return HunkRange{Start: start, Count: count}, true
}

// stripBPrefix removes the leading "b/" added by git in diff headers.
// "+++ b/path/to/file" → "path/to/file"
func stripBPrefix(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "b/") {
		return s[2:]
	}
	return s
}
