package tui

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"lazyslurm/internal/config"
	"lazyslurm/internal/history"
	"lazyslurm/internal/slurm"
	"path/filepath"
)

func fakeRunner(outputs map[string]string) slurm.Runner {
	return func(name string, args ...string) (string, error) {
		if out, ok := outputs[name]; ok {
			return out, nil
		}
		return "", nil
	}
}

func TestSmokeView(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.LogDir = filepath.Join(dir, "logs")
	cfg.HistoryFile = filepath.Join(dir, "history.json")
	cfg.Resolve()
	store := history.New(cfg.HistoryFile)
	run := fakeRunner(map[string]string{
		"squeue":   "42|exp1|alice|R|RUNNING|00:01:00|01:00:00|1|4|8G|gpu|node01|normal|2026-09-26T10:01:00|2026-09-26T10:00:00\n",
		"scontrol": "JobId=42 JobState=RUNNING Reason=None NumNodes=1 NumCPUs=4\n",
	})
	m := New(cfg, store, run)
	um, _ := m.Update(tea.WindowSizeMsg{Width: 150, Height: 45})
	m = um.(Model)
	um, _ = m.Update(jobsMsg{jobs: mustParse(t), err: nil})
	m = um.(Model)
	if len(m.visibleJobs()) != 1 {
		t.Fatalf("expected 1 job, got %d", len(m.visibleJobs()))
	}
	v := m.View()
	for _, want := range []string{"Submissions", "Jobs", "Details", "Logs", "42", "8G"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view missing %q", want)
		}
	}
	// Log target resolves into our log dir.
	if _, p := m.refreshLogTarget(); !strings.Contains(p, "42") {
		t.Fatalf("bad log path %q", p)
	}
}

func mustParse(t *testing.T) []slurm.Job {
	t.Helper()
	jobs, err := slurm.ParseSqueue("42|exp1|alice|R|RUNNING|00:01:00|01:00:00|1|4|8G|gpu|node01|normal|2026-09-26T10:01:00|2026-09-26T10:00:00\n")
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestCycleCPUs(t *testing.T) {
	m := Model{cfg: config.Default(), submitInputs: makeSubmitInputs(config.Default())}
	val := func() string { return m.submitInputs[fieldCPUs].Value() }
	if val() != "8" {
		t.Fatalf("default CPUs = %q, want 8", val())
	}
	// Step through the whole 8..192 range and wrap around.
	for want := 16; want <= 192; want += 8 {
		m.cycleCPUs(1)
		if val() != strconv.Itoa(want) {
			t.Fatalf("cycle+ = %q, want %d", val(), want)
		}
	}
	m.cycleCPUs(1)
	if val() != "8" {
		t.Fatalf("cycle wrap = %q, want 8", val())
	}
	m.cycleCPUs(-1)
	if val() != "192" {
		t.Fatalf("cycle- = %q, want 192", val())
	}
}

func TestPickLogStream(t *testing.T) {
	// Preferred stream wins when non-empty.
	if c, s := pickLogStream(false, "out", "err"); c != "out" || s != ".out" {
		t.Fatalf("got %q %q", c, s)
	}
	if c, s := pickLogStream(true, "out", "err"); c != "err" || s != ".err" {
		t.Fatalf("got %q %q", c, s)
	}
	// Empty stdout (typical failed job) falls back to stderr with banner.
	c, s := pickLogStream(false, "", "boom\n")
	if s != ".err" || c == "" || !strings.Contains(c, "boom") || !strings.Contains(c, "stdout empty") {
		t.Fatalf("fallback: %q %q", c, s)
	}
	// Mirror case.
	c, s = pickLogStream(true, "hello\n", "")
	if s != ".out" || !strings.Contains(c, "hello") || !strings.Contains(c, "stderr empty") {
		t.Fatalf("mirror: %q %q", c, s)
	}
	// Both empty.
	if c, s := pickLogStream(false, "", ""); c != "" || s != "" {
		t.Fatalf("both empty: %q %q", c, s)
	}
	// Whitespace-only counts as content (not our call to trim job output).
	if c, s := pickLogStream(false, "\n", ""); c != "\n" || s != ".out" {
		t.Fatalf("blank: %q %q", c, s)
	}
}

func TestFilter(t *testing.T) {
	cfg := config.Default()
	m := New(cfg, nil, fakeRunner(nil))
	m.jobs = []slurm.Job{{ID: "1", Name: "train", State: "RUNNING"}, {ID: "2", Name: "eval", State: "PENDING"}}
	m.filter = "train"
	if len(m.visibleJobs()) != 1 {
		t.Fatalf("filter failed: %+v", m.visibleJobs())
	}
}

func TestWindowRows(t *testing.T) {
	// Fits: no footer.
	if s, e, f := windowRows(0, 0, 3, 5); s != 0 || e != 3 || f != "" {
		t.Fatalf("fits: %d %d %q", s, e, f)
	}
	// Overflow keeps selection visible with a footer.
	s, e, f := windowRows(0, 9, 10, 4)
	if f == "" {
		t.Fatal("expected footer")
	}
	if s > 9 || 9 >= e {
		t.Fatalf("selection outside window: %d %d", s, e)
	}
	if e-s > 3 {
		t.Fatalf("window exceeds budget: %d %d", s, e)
	}
	// Scrolling up brings the window along.
	if s, _, _ := windowRows(6, 2, 10, 4); s != 2 {
		t.Fatalf("scroll up: start=%d", s)
	}
}

func sizedModel(t *testing.T, w, h int) Model {
	t.Helper()
	m := New(config.Default(), nil, fakeRunner(nil))
	um, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return um.(Model)
}

// assertFrame checks the two properties every View must uphold: no line is
// wider than the terminal and the frame is never taller than the terminal.
func assertFrame(t *testing.T, v string, w, h int) {
	t.Helper()
	if v == "" {
		return
	}
	lines := strings.Split(v, "\n")
	if len(lines) > h {
		t.Fatalf("frame is %d lines tall, terminal is %d\n%s", len(lines), h, v)
	}
	for i, line := range lines {
		if got := lipgloss.Width(line); got > w {
			t.Fatalf("line %d is %d cells wide, terminal is %d: %q", i, got, w, line)
		}
	}
}

func populatedModel(t *testing.T, w, h int) Model {
	t.Helper()
	m := sizedModel(t, w, h)
	for i := 0; i < 40; i++ {
		m.submissions = append(m.submissions, history.Submission{
			Timestamp: time.Now(),
			Command:   "python train.py --epochs 10 --a-very-long-command-that-needs-to-be-truncated " + strconv.Itoa(i),
			JobName:   "job-with-a-long-name-" + strconv.Itoa(i),
			JobIDs:    []string{strconv.Itoa(1000 + i)},
			WorkDir:   "/very/long/path/to/a/working/directory/that/does/not/fit/in/a/narrow/panel",
			CPUs:      8, Time: "01:00:00",
		})
		m.jobs = append(m.jobs, slurm.Job{
			ID: strconv.Itoa(1000 + i), Name: "job-with-a-long-name-" + strconv.Itoa(i),
			User: "alice", State: "RUNNING", StateShort: "R", CPUs: "8", Mem: "8G",
			Partition: "gpu", NodeList: "a-very-long-nodelist-name",
		})
	}
	m.selSub, m.selJob = 39, 39
	m.clampSelections()
	return m
}

func TestViewFitsAllTerminalSizesAndModes(t *testing.T) {
	sizes := [][2]int{
		{200, 60}, {150, 40}, {120, 30}, {100, 25}, {99, 24},
		{80, 24}, {64, 20}, {63, 20}, {60, 18}, {50, 15},
		{40, 12}, {32, 10}, {30, 8}, {20, 5}, {19, 5}, {10, 3},
		{3, 3}, {2, 5}, {1, 1},
	}
	for _, sz := range sizes {
		w, h := sz[0], sz[1]
		base := populatedModel(t, w, h)
		for _, focus := range []Panel{PanelSubmissions, PanelJobs, PanelLogs} {
			for _, mode := range []Mode{ModeNormal, ModeSubmit, ModeConfirm, ModeHelp} {
				m := base
				m.focused = focus
				m.mode = mode
				switch mode {
				case ModeSubmit:
					m.openSubmit()
				case ModeConfirm:
					m.confirmJobID = "1000"
				case ModeHelp:
					m.helpOff = 1000 // out-of-range offsets must be clamped
				}
				assertFrame(t, m.View(), w, h)
			}
		}
		// Filter input adds another status line.
		m := base
		m.filtering = true
		m.filterInput.Focus()
		assertFrame(t, m.View(), w, h)
	}
}

func TestLayoutInvariants(t *testing.T) {
	for _, sz := range [][2]int{{160, 50}, {120, 35}, {100, 24}, {80, 20}, {64, 16}, {50, 14}, {35, 10}, {24, 6}} {
		m := sizedModel(t, sz[0], sz[1])
		l := m.computeLayout()
		if l.tooSmall {
			continue
		}
		if got := l.headerH + l.bodyH + l.logH + l.statusH; got != l.height {
			t.Fatalf("%dx%d: vertical budget %d != height %d (%+v)", sz[0], sz[1], got, l.height, l)
		}
		switch l.mode {
		case bodyWide:
			if l.leftW+l.jobsW+l.detailsW != l.width {
				t.Fatalf("%dx%d: wide widths %d+%d+%d != %d", sz[0], sz[1], l.leftW, l.jobsW, l.detailsW, l.width)
			}
		case bodyMedium:
			if l.leftW+l.jobsW != l.width {
				t.Fatalf("%dx%d: medium widths %d+%d != %d", sz[0], sz[1], l.leftW, l.jobsW, l.width)
			}
			if l.jobsH+l.detailsH != l.bodyH {
				t.Fatalf("%dx%d: medium heights %d+%d != %d", sz[0], sz[1], l.jobsH, l.detailsH, l.bodyH)
			}
		default:
			if l.leftH+l.jobsH+l.detailsH != l.bodyH {
				t.Fatalf("%dx%d: narrow heights %d+%d+%d != %d", sz[0], sz[1], l.leftH, l.jobsH, l.detailsH, l.bodyH)
			}
		}
	}
}

func TestResizeKeepsSelectionVisible(t *testing.T) {
	m := populatedModel(t, 150, 45)
	m.focused = PanelJobs
	m.selJob = 39
	m.clampSelections()

	for _, sz := range [][2]int{{80, 20}, {50, 14}, {140, 50}, {64, 12}, {150, 45}} {
		um, _ := m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		m = um.(Model)
		l := m.computeLayout()
		_, jobRows := m.listBudgets()
		if jobRows > 0 {
			start, end, _ := windowRows(m.jobOff, m.selJob, len(m.visibleJobs()), jobRows)
			if m.selJob < start || m.selJob >= end {
				t.Fatalf("%dx%d: selection %d outside visible window [%d,%d) rows=%d",
					sz[0], sz[1], m.selJob, start, end, jobRows)
			}
		}
		if l.bodyH > 0 {
			assertFrame(t, m.View(), sz[0], sz[1])
		}
	}
}

func TestViewFitsHeight(t *testing.T) {
	m := sizedModel(t, 150, 40)
	for i := 0; i < 30; i++ {
		m.submissions = append(m.submissions, history.Submission{
			Timestamp: time.Now(), Command: "echo " + strconv.Itoa(i),
			JobName: "job", JobIDs: []string{strconv.Itoa(1000 + i)},
		})
		m.jobs = append(m.jobs, slurm.Job{
			ID: strconv.Itoa(1000 + i), Name: "job", State: "RUNNING",
			StateShort: "R", CPUs: "8", Mem: "1G",
		})
	}
	m.selSub = 29
	m.selJob = 29
	m.clampSelections()
	v := m.View()
	if n := strings.Count(v, "\n") + 1; n > 40 {
		t.Fatalf("view overflows window: %d lines for height 40", n)
	}
}

func TestLogLastLineVisible(t *testing.T) {
	m := sizedModel(t, 150, 40)
	m.jobs = []slurm.Job{{ID: "42", Name: "exp1", State: "RUNNING", StateShort: "R"}}
	m.selJob = 0
	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, "L"+strconv.Itoa(i))
	}
	um, _ := m.Update(logsMsg{jobID: "42", outPath: "o", errPath: "e",
		out: strings.Join(lines, "\n")})
	m = um.(Model)
	if got := m.logView.View(); !strings.Contains(got, "L20") {
		t.Fatalf("last line missing from viewport")
	}
	if v := m.View(); !strings.Contains(v, "L20") {
		t.Fatalf("last line missing from full view")
	}
}

func TestLongLogLinesFitAcrossResizes(t *testing.T) {
	m := sizedModel(t, 120, 30)
	m.jobs = []slurm.Job{{ID: "42", Name: "exp1", State: "RUNNING", StateShort: "R"}}
	m.selJob = 0
	long := strings.Repeat("wide-log-line-", 40)
	content := long + "\n" + long + "\ntail-line"
	um, _ := m.Update(logsMsg{jobID: "42", outPath: "o", errPath: "e", out: content})
	m = um.(Model)

	for _, sz := range [][2]int{{120, 30}, {70, 20}, {40, 14}, {60, 10}, {120, 30}} {
		um, _ := m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		m = um.(Model)
		// Follow mode must keep the newest line visible after every resize.
		if v := m.View(); !strings.Contains(v, "tail-line") {
			t.Fatalf("%dx%d: last log line not visible", sz[0], sz[1])
		}
		assertFrame(t, m.View(), sz[0], sz[1])
	}
}

func TestHistoryEnterLoadsLog(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.LogDir = dir
	cfg.HistoryFile = filepath.Join(dir, "history.json")
	m := sizedModel(t, 150, 40)
	m.cfg = cfg
	m.submissions = []history.Submission{{
		Timestamp: time.Now(), Command: "./old.sh", JobName: "oldjob",
		JobIDs: []string{"777"},
	}}
	m.selSub = 0
	m.focused = PanelSubmissions
	if err := os.WriteFile(filepath.Join(dir, "oldjob-777.out"), []byte("archived-stdout"), 0o644); err != nil {
		t.Fatal(err)
	}

	um, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = um.(Model)
	if cmd == nil {
		t.Fatal("expected log fetch command")
	}
	um, _ = m.Update(cmd())
	m = um.(Model)
	if m.logPinned != "777" {
		t.Fatalf("pinned = %q", m.logPinned)
	}
	if got := m.logView.View(); !strings.Contains(got, "archived-stdout") {
		t.Fatalf("archived log not shown: %q", got)
	}
	if v := m.View(); !strings.Contains(v, "finished") {
		t.Fatalf("details should mark job finished")
	}

	// Ticks must not clobber the pinned view with live (empty) logs.
	um, _ = m.Update(logTickMsg(time.Now()))
	m = um.(Model)
	if m.logPinned != "777" {
		t.Fatal("tick cleared pinned view")
	}

	// Missing files: clear error, no pin.
	m2 := sizedModel(t, 150, 40)
	m2.cfg = cfg
	m2.submissions = []history.Submission{{
		Timestamp: time.Now(), Command: "./gone.sh", JobName: "gone",
		JobIDs: []string{"888"},
	}}
	m2.focused = PanelSubmissions
	um2, cmd2 := m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 = um2.(Model)
	if cmd2 != nil {
		t.Fatal("expected no fetch without log files")
	}
	if m2.logPinned != "" || !strings.Contains(m2.statusMsg, "no log file") {
		t.Fatalf("pinned=%q status=%q", m2.logPinned, m2.statusMsg)
	}
}
