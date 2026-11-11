package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
)

// File completion for the Command input.
//
// The TUI runs on the cluster login node, so completing against the local
// working directory completes cluster paths: ./myscript.sh, sub/dir/…,
// ~/proj/…, /absolute/…. Repeated Tab cycles through the matches.

// resetComp clears pending completion state.
func (m *Model) resetComp() {
	m.compBase = ""
	m.compMatches = nil
	m.compIdx = -1
}

// doComplete performs one Tab completion step on the Command input:
// single match inserts it, several matches insert the longest common
// prefix (or list candidates), and further Tabs cycle through them.
func (m *Model) doComplete() {
	if len(m.submitInputs) == 0 {
		return
	}
	ti := &m.submitInputs[0]
	val := ti.Value()
	pos := ti.Position()
	runes := []rune(val)
	if pos < 0 {
		pos = 0
	}
	if pos > len(runes) {
		pos = len(runes)
	}
	start, tok := tokenAt(val, pos)
	cwd, _ := os.Getwd()

	// Keep cycling while the token is untouched since the list was shown
	// (tok == base) or equals the candidate just inserted.
	if len(m.compMatches) > 0 && (tok == m.compBase ||
		(m.compIdx >= 0 && tok == m.compMatches[m.compIdx])) {
		if m.compIdx < 0 || tok == m.compBase {
			m.compIdx = 0
		} else {
			m.compIdx = (m.compIdx + 1) % len(m.compMatches)
		}
		m.applyCandidate(ti, runes, start, pos, m.compMatches[m.compIdx])
		return
	}

	matches := completeFiles(tok, cwd)
	switch len(matches) {
	case 0:
		m.resetComp()
		m.setStatus(fmt.Sprintf("no file match for %q", tok), true)
	case 1:
		m.resetComp()
		m.applyCandidate(ti, runes, start, pos, matches[0])
	default:
		if lcp := longestCommonPrefix(matches); runeLen(lcp) > runeLen(tok) {
			// Unambiguous extension: insert it, recompute on next Tab.
			m.resetComp()
			m.applyCandidate(ti, runes, start, pos, lcp)
		} else {
			m.compBase = tok
			m.compMatches = matches
			m.compIdx = -1
			m.setStatus(fmt.Sprintf("%d matches: %s  (Tab to cycle)",
				len(matches), truncate(strings.Join(matches, " "), 160)), false)
		}
	}
}

func (m *Model) applyCandidate(ti *textinput.Model, runes []rune, start, pos int, cand string) {
	ti.SetValue(string(runes[:start]) + cand + string(runes[pos:]))
	ti.SetCursor(start + runeLen(cand))
}

// tokenAt returns the start rune-offset and text of the whitespace/shell
// token ending at cursor position pos.
func tokenAt(value string, pos int) (int, string) {
	runes := []rune(value)
	if pos < 0 {
		pos = 0
	}
	if pos > len(runes) {
		pos = len(runes)
	}
	start := pos
	for start > 0 && !isTokSep(runes[start-1]) {
		start--
	}
	return start, string(runes[start:pos])
}

func isTokSep(r rune) bool {
	switch r {
	case ' ', '\t', '\n', ';', '|', '&', '(', ')', '<', '>':
		return true
	}
	return false
}

// completeFiles returns sorted replacement tokens for tok: the typed text
// up to the last slash (plus a preserved leading quote) joined with each
// matching entry; directories get a trailing slash. An executable file
// completed by bare name (no slash typed) gets a ./ prefix so the
// completed command actually runs instead of failing PATH lookup.
func completeFiles(tok, cwd string) []string {
	quote := ""
	t := tok
	if len(t) > 0 && (t[0] == '"' || t[0] == '\'') {
		quote = t[:1]
		t = t[1:]
	}
	dirTyped, filePrefix := "", t
	if i := strings.LastIndexByte(t, '/'); i >= 0 {
		dirTyped = t[:i+1]
		filePrefix = t[i+1:]
	}
	// Bare "~" means $HOME.
	if t == "~" {
		dirTyped = "~/"
		filePrefix = ""
	}
	var listDir string
	switch {
	case dirTyped == "":
		listDir = cwd
	case dirTyped == "~/" || strings.HasPrefix(dirTyped, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		listDir = filepath.Join(home, strings.TrimPrefix(dirTyped, "~"))
	case strings.HasPrefix(dirTyped, "~"):
		return nil // ~otheruser: not resolved
	case filepath.IsAbs(dirTyped):
		listDir = dirTyped
	default:
		listDir = filepath.Join(cwd, dirTyped)
	}
	entries, err := os.ReadDir(listDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, filePrefix) {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(filePrefix, ".") {
			continue
		}
		cand := quote + dirTyped + name
		if e.IsDir() {
			cand += "/"
		} else if dirTyped == "" && isExecutable(listDir, name) {
			cand = quote + "./" + name
		}
		out = append(out, cand)
	}
	sort.Strings(out)
	return out
}

// isExecutable reports whether name inside dir has any execute bit set
// (follows symlinks).
func isExecutable(dir, name string) bool {
	st, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	return !st.IsDir() && st.Mode().Perm()&0o111 != 0
}

func longestCommonPrefix(strs []string) string {
	if len(strs) == 0 {
		return ""
	}
	prefix := []rune(strs[0])
	for _, s := range strs[1:] {
		r := []rune(s)
		i := 0
		for i < len(prefix) && i < len(r) && prefix[i] == r[i] {
			i++
		}
		prefix = prefix[:i]
		if len(prefix) == 0 {
			return ""
		}
	}
	return string(prefix)
}

func runeLen(s string) int { return len([]rune(s)) }
