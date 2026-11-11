package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"lazyslurm/internal/config"
)

func TestTokenAt(t *testing.T) {
	cases := []struct {
		val  string
		pos  int
		want string
	}{
		{"./benzene_au.sh", 15, "./benzene_au.sh"},
		{"sbatch ./benz", 13, "./benz"},
		{"sbatch ./benz", 9, "./"},
		{"a;./run", 7, "./run"},
		{"", 0, ""},
		{"abc", 99, "abc"},
	}
	for _, c := range cases {
		_, got := tokenAt(c.val, c.pos)
		if got != c.want {
			t.Fatalf("tokenAt(%q,%d) = %q, want %q", c.val, c.pos, got, c.want)
		}
	}
}

func TestCompleteFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"benzene_au.sh", "run_a.sh", "run_b.sh", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := completeFiles("./benz", dir); !reflect.DeepEqual(got, []string{"./benzene_au.sh"}) {
		t.Fatalf("single: %v", got)
	}
	if got := completeFiles("run", dir); !reflect.DeepEqual(got, []string{"run_a.sh", "run_b.sh"}) {
		t.Fatalf("multi: %v", got)
	}
	if got := completeFiles("scri", dir); !reflect.DeepEqual(got, []string{"scripts/"}) {
		t.Fatalf("dir gets slash: %v", got)
	}
	if got := completeFiles("", dir); len(got) != 5 {
		t.Fatalf("empty lists non-dotfiles, got %v", got)
	}
	if got := completeFiles(".", dir); len(got) != 1 || got[0] != ".hidden" {
		t.Fatalf("dot prefix: %v", got)
	}
	if got := completeFiles("\"benz", dir); !reflect.DeepEqual(got, []string{"\"benzene_au.sh"}) {
		t.Fatalf("quote preserved: %v", got)
	}
	if got := completeFiles("nomatch", dir); len(got) != 0 {
		t.Fatalf("expected none, got %v", got)
	}
	// Executables completed by bare name get ./ so they run directly.
	if err := os.WriteFile(filepath.Join(dir, "tool"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := completeFiles("to", dir); !reflect.DeepEqual(got, []string{"./tool"}) {
		t.Fatalf("exec: %v", got)
	}
	// …but not when a slash was already typed.
	if got := completeFiles("./to", dir); !reflect.DeepEqual(got, []string{"./tool"}) {
		t.Fatalf("exec with slash: %v", got)
	} // Absolute subdir token.
	sub := filepath.Join(dir, "scripts")
	if err := os.WriteFile(filepath.Join(sub, "job.sh"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := completeFiles(sub+"/j", dir); !reflect.DeepEqual(got, []string{sub + "/job.sh"}) {
		t.Fatalf("abs: %v", got)
	}
}

func TestLongestCommonPrefix(t *testing.T) {
	if got := longestCommonPrefix([]string{"run_a.sh", "run_b.sh"}); got != "run_" {
		t.Fatalf("got %q", got)
	}
	if got := longestCommonPrefix([]string{"only"}); got != "only" {
		t.Fatalf("got %q", got)
	}
	if got := longestCommonPrefix(nil); got != "" {
		t.Fatalf("got %q", got)
	}
}

// Model-level: full Tab flow against a temp working directory.
func TestDoCompleteFlow(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"benzene_au.sh", "run_a.sh", "run_b.sh"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	m := Model{cfg: config.Default(), submitInputs: makeSubmitInputs(config.Default())}
	cmd := &m.submitInputs[0]
	cmd.Focus()

	// Single match completes in one Tab.
	cmd.SetValue("./benz")
	m.doComplete()
	if cmd.Value() != "./benzene_au.sh" {
		t.Fatalf("single complete: %q", cmd.Value())
	}

	// Ambiguous: first Tab extends to common prefix…
	cmd.SetValue("run")
	m.doComplete()
	if cmd.Value() != "run_" {
		t.Fatalf("lcp extend: %q", cmd.Value())
	}
	// …second Tab lists, third/fourth cycle.
	m.doComplete()
	if m.statusMsg == "" || len(m.compMatches) != 2 {
		t.Fatalf("list: %q %+v", m.statusMsg, m.compMatches)
	}
	m.doComplete()
	if cmd.Value() != "run_a.sh" {
		t.Fatalf("cycle 1: %q", cmd.Value())
	}
	m.doComplete()
	if cmd.Value() != "run_b.sh" {
		t.Fatalf("cycle 2: %q", cmd.Value())
	}
	m.doComplete()
	if cmd.Value() != "run_a.sh" {
		t.Fatalf("cycle wrap: %q", cmd.Value())
	}
}
