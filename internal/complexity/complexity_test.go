package complexity_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yourname/blast-radius/internal/complexity"
)

func TestOfFunction(t *testing.T) {
	src := `package p

func simple() {}

func withBranches(n int) int {
	if n > 0 {
		for i := 0; i < n; i++ {
			if i%2 == 0 || i > 5 {
				n--
			}
		}
	}
	return n
}
`
	f, err := os.CreateTemp(t.TempDir(), "*.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(src); err != nil {
		t.Fatal(err)
	}
	f.Close()
	path := filepath.Clean(f.Name())

	if got := complexity.OfFunction(path, 3); got != 1 {
		t.Errorf("simple(): want 1, got %d", got)
	}
	// withBranches: if + for + if + || = 4 branches → complexity 5
	if got := complexity.OfFunction(path, 6); got != 5 {
		t.Errorf("withBranches(): want 5, got %d", got)
	}
	// Line that doesn't exist in any function → 1
	if got := complexity.OfFunction(path, 999); got != 1 {
		t.Errorf("no match: want 1, got %d", got)
	}
}
