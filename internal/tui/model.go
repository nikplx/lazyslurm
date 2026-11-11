package tui

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"lazyslurm/internal/config"
	"lazyslurm/internal/history"
	"lazyslurm/internal/slurm"
)

// Panel is the focused pane.
type Panel int

const (
	PanelSubmissions Panel = iota
	PanelJobs
	PanelLogs
)

// Mode is the input mode / popup state.
type Mode int

const (
	ModeNormal Mode = iota
	ModeSubmit
	ModeConfirm
	ModeHelp
)

// Messages -------------------------------------------------------------

type tickMsg time.Time
type logTickMsg time.Time

type jobsMsg struct {
	jobs []slurm.Job
	err  error
}

// logsMsg carries the tailed contents of both streams. Either file may be
// missing (pending jobs) or empty (failed jobs often have empty stdout).
type logsMsg struct {
	jobID   string
	outPath string
	errPath string
	out     string
	errOut  string
	outErr  error
	errRead error
}

type submitDoneMsg struct {
	sub   history.Submission
	jobID string
	raw   string
	err   error
}

type cancelDoneMsg struct {
	jobID string
	raw   string
	err   error
}

type scontrolMsg struct {
	jobID string
	info  map[string]string
}

// Model ----------------------------------------------------------------

type Model struct {
	cfg   config.Config
	store *history.Store
	run   slurm.Runner

	submissions []history.Submission
	jobs        []slurm.Job

	focused Panel
	mode    Mode
	ready   bool
	width   int
	height  int

	selSub int
	selJob int // index into visibleJobs()
	subOff int // scroll offsets for capped panes
	jobOff int

	statusMsg string
	statusErr bool
	statusAt  time.Time

	// log viewer (both streams are tailed; the pane shows the preferred
	// one, falling back to the other when it is empty)
	logView     viewport.Model
	logPath     string // displayed stream's path (yank/details compat)
	logOutPath  string
	logErrPath  string
	logOutLines int
	logErrLines int
	logStream   string // ".out" or ".err" actually shown, "" when both empty
	logJobID    string
	logFollow   bool
	logWantErr  bool
	// Pinned (archived) log from history for a job no longer in squeue.
	// While set, the 1s auto-tail leaves the pane alone.
	logPinned    string
	logPinnedOut string
	logPinnedErr string

	// submit form
	submitInputs []textinput.Model
	submitFocus  int // 0..len(inputs); len(inputs) == submit button
	submitting   bool

	// Tab completion state for the Command input
	compBase    string
	compMatches []string
	compIdx     int

	// cancel confirm
	confirmJobID string

	// filter
	filter      string
	filtering   bool
	filterInput textinput.Model

	// details
	scontrolJobID string
	scontrolInfo  map[string]string

	helpOff int // scroll offset for the (possibly taller-than-screen) help popup

	lastJobKey string // to detect selection change for scontrol refresh
}

// New builds the model. History load failures are surfaced in the status bar.
func New(cfg config.Config, store *history.Store, run slurm.Runner) Model {
	if run == nil {
		run = slurm.DefaultRunner
	}
	m := Model{
		cfg:       cfg,
		store:     store,
		run:       run,
		focused:   PanelJobs,
		logFollow: true,
		logView:   viewport.New(80, 20),
	}
	m.logView.SetContent("Select a job and press enter to view its log.")

	m.submitInputs = makeSubmitInputs(cfg)
	fi := textinput.New()
	fi.Placeholder = "filter: id, name, state, partition…"
	fi.CharLimit = 128
	fi.Width = 40
	m.filterInput = fi

	if store != nil {
		subs, err := store.List()
		if err != nil {
			m.setStatus(fmt.Sprintf("history: %v", err), true)
		} else {
			m.submissions = subs
		}
	}
	return m
}

func makeSubmitInputs(cfg config.Config) []textinput.Model {
	mk := func(ph, val string, limit int) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = ph
		ti.SetValue(val)
		ti.CharLimit = limit
		ti.Width = 60
		return ti
	}
	cpus := mk("cpus per task", strconv.Itoa(slurm.NormalizeCPUs(cfg.DefaultCPUs)), 3)
	cpus.Width = 8 // stepped 8..192 selector, not free text (see cycleCPUs)
	return []textinput.Model{
		mk("e.g. python train.py --epochs 10", "", 4096),                    // 0 command
		mk("job name", cfg.DefaultJobName, 64),                              // 1 name
		mk("partition (empty = cluster default)", cfg.DefaultPartition, 64), // 2 partition
		cpus, // 3 cpus selector
		mk("time limit, e.g. 01:00:00", cfg.DefaultTime, 32), // 4 time
	}
}

// Submit field indices.
const (
	fieldCommand = iota
	fieldJobName
	fieldPartition
	fieldCPUs
	fieldTime
)

func (m *Model) setStatus(s string, isErr bool) {
	m.statusMsg = s
	m.statusErr = isErr
	m.statusAt = time.Now()
}

// Commands ---------------------------------------------------------------

func (m Model) pollInterval() time.Duration {
	if m.cfg.PollSecs <= 0 {
		return 2 * time.Second
	}
	return time.Duration(m.cfg.PollSecs) * time.Second
}

func (m Model) tickCmd() tea.Cmd {
	return tea.Tick(m.pollInterval(), func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) logTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return logTickMsg(t) })
}

func (m Model) fetchJobsCmd() tea.Cmd {
	run := m.run
	userOnly := m.cfg.UserOnly
	return func() tea.Msg {
		jobs, _, err := slurm.FetchJobs(run, userOnly)
		return jobsMsg{jobs: jobs, err: err}
	}
}

// readTail reads the last ~256KB / 3000 lines of path.
// ok=false when the file could not be read at all.
func readTail(path string) (content string, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	// Cap memory: keep last ~256KB / 3000 lines.
	if len(data) > 256*1024 {
		data = data[len(data)-256*1024:]
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 3000 {
		lines = lines[len(lines)-3000:]
	}
	return strings.Join(lines, "\n"), true, nil
}

func (m Model) fetchLogsCmd(jobID, outPath, errPath string) tea.Cmd {
	return func() tea.Msg {
		out, outOK, outErr := readTail(outPath)
		errOut, errOK, errRead := readTail(errPath)
		// Normalize "missing" to empty content; keep real read errors.
		if !outOK && os.IsNotExist(outErr) {
			outErr = nil
		}
		if !errOK && os.IsNotExist(errRead) {
			errRead = nil
		}
		_ = outOK
		_ = errOK
		return logsMsg{jobID: jobID, outPath: outPath, errPath: errPath,
			out: out, errOut: errOut, outErr: outErr, errRead: errRead}
	}
}

// pickLogStream chooses what the log pane displays. The preferred stream
// wins unless it is empty while the other has content — the typical
// failed-job case (empty stdout, error in stderr) — which falls back with
// a banner. Empty return means both streams are empty.
func pickLogStream(wantErr bool, out, errOut string) (content, stream string) {
	if !wantErr {
		if out != "" {
			return out, ".out"
		}
		if errOut != "" {
			return "── stdout empty, showing stderr (e to toggle) ──\n" + errOut, ".err"
		}
		return "", ""
	}
	if errOut != "" {
		return errOut, ".err"
	}
	if out != "" {
		return "── stderr empty, showing stdout (e to toggle) ──\n" + out, ".out"
	}
	return "", ""
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func (m Model) fetchScontrolCmd(jobID string) tea.Cmd {
	run := m.run
	return func() tea.Msg {
		info, err := slurm.FetchScontrol(run, jobID)
		if err != nil {
			return scontrolMsg{jobID: jobID, info: nil}
		}
		return scontrolMsg{jobID: jobID, info: info}
	}
}

func (m Model) submitCmd(o slurm.SubmitOptions, sub history.Submission) tea.Cmd {
	run := m.run
	store := m.store
	return func() tea.Msg {
		id, raw, err := slurm.SubmitJob(run, o)
		if err != nil {
			return submitDoneMsg{sub: sub, err: err, raw: raw}
		}
		sub.JobIDs = []string{id}
		if store != nil {
			if _, aerr := store.Append(sub); aerr != nil {
				return submitDoneMsg{sub: sub, jobID: id, raw: raw + " (history save failed: " + aerr.Error() + ")", err: nil}
			}
		}
		return submitDoneMsg{sub: sub, jobID: id, raw: raw}
	}
}

func (m Model) cancelCmd(jobID string) tea.Cmd {
	run := m.run
	return func() tea.Msg {
		raw, err := slurm.CancelJob(run, jobID)
		return cancelDoneMsg{jobID: jobID, raw: raw, err: err}
	}
}

// Init -------------------------------------------------------------------

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.tickCmd(), m.logTickCmd(), m.fetchJobsCmd())
}

// Helpers -----------------------------------------------------------------

func (m *Model) visibleJobs() []slurm.Job {
	if m.filter == "" {
		return m.jobs
	}
	f := strings.ToLower(m.filter)
	out := make([]slurm.Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		hay := strings.ToLower(j.ID + " " + j.Name + " " + j.State + " " + j.StateShort + " " + j.Partition + " " + j.User + " " + j.NodeList)
		if strings.Contains(hay, f) {
			out = append(out, j)
		}
	}
	return out
}

func (m *Model) selectedJob() *slurm.Job {
	v := m.visibleJobs()
	if len(v) == 0 || m.selJob < 0 || m.selJob >= len(v) {
		return nil
	}
	return &v[m.selJob]
}

func (m *Model) selectedSubmission() *history.Submission {
	if len(m.submissions) == 0 || m.selSub < 0 || m.selSub >= len(m.submissions) {
		return nil
	}
	return &m.submissions[m.selSub]
}

func (m *Model) clampSelections() {
	if m.selSub < 0 {
		m.selSub = 0
	}
	if len(m.submissions) > 0 && m.selSub >= len(m.submissions) {
		m.selSub = len(m.submissions) - 1
	}
	if m.selJob < 0 {
		m.selJob = 0
	}
	if n := len(m.visibleJobs()); n > 0 && m.selJob >= n {
		m.selJob = n - 1
	}
	m.ensureVisible()
}

// bodyMode selects how the three main panes are arranged.
type bodyMode int

const (
	bodyWide        bodyMode = iota // submissions | jobs | details
	bodyMedium                      // submissions | jobs above details
	bodyNarrowStack                 // submissions above jobs (details only if tall)
	bodyNarrowFocus                 // only the focused list is shown
)

// layout is the single source of truth for screen geometry. Every pane and
// popup is sized from it; nothing derives its own hard-coded dimensions.
type layout struct {
	width, height int
	headerH       int
	statusH       int
	bodyH         int
	logH          int
	showLog       bool

	leftW, jobsW, detailsW int
	leftH, jobsH, detailsH int

	mode     bodyMode
	tooSmall bool
}

const (
	// Below this the layout cannot usefully show panes; a short hint is
	// rendered instead of letting boxes spill over the terminal.
	minLayoutWidth  = 20
	minLayoutHeight = 5
)

func clampInt(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// computeLayout derives a responsive layout from the current terminal size.
// It guarantees header + body + log + status == height and that all pane
// widths add up to width. Font-size changes arrive as WindowSizeMsg and flow
// through here just like ordinary resizes.
func (m Model) computeLayout() layout {
	l := layout{width: m.width, height: m.height}
	if l.width <= 0 || l.height <= 0 {
		l.tooSmall = true
		return l
	}
	if l.width < minLayoutWidth || l.height < minLayoutHeight {
		l.tooSmall = true
		return l
	}

	l.headerH = 1
	if l.height >= 12 {
		l.statusH = 2
	} else {
		l.statusH = 1
	}
	contentH := l.height - l.headerH - l.statusH
	if contentH < 2 {
		l.tooSmall = true
		return l
	}

	// Log pane: roughly a third, but never at the expense of a usable list.
	// On very short terminals keep the body and hide logs; if logs have focus,
	// give them the whole content area instead.
	switch {
	case m.focused == PanelLogs && contentH < 8:
		l.showLog = true
		l.logH = contentH
		l.bodyH = 0
	case contentH >= 8:
		l.showLog = true
		l.logH = contentH / 3
		if l.logH < 4 {
			l.logH = 4
		}
		if l.logH > contentH-4 {
			l.logH = contentH - 4
		}
		if l.logH < 3 {
			l.logH = 3
		}
		l.bodyH = contentH - l.logH
	default:
		l.showLog = false
		l.bodyH = contentH
	}

	// Horizontal arrangement. Widths always sum to m.width exactly.
	if l.width >= 100 {
		l.mode = bodyWide
		l.leftW = clampInt(l.width*30/100, 24, 52)
		right := l.width - l.leftW
		l.detailsW = clampInt(right*34/100, 18, 40)
		l.jobsW = right - l.detailsW
		if l.jobsW < 24 {
			l.jobsW = right
			l.detailsW = 0
		}
		l.leftH, l.jobsH, l.detailsH = l.bodyH, l.bodyH, l.bodyH
	} else if l.width >= 60 {
		l.mode = bodyMedium
		l.leftW = clampInt(l.width*35/100, 20, 36)
		l.jobsW = l.width - l.leftW
		l.leftH = l.bodyH
		// Stack jobs above details when there is room for both.
		if l.bodyH >= 9 {
			jobsH := l.bodyH * 55 / 100
			if jobsH < 4 {
				jobsH = 4
			}
			detailsH := l.bodyH - jobsH
			if detailsH >= 4 {
				l.jobsH, l.detailsH = jobsH, detailsH
				l.detailsW = l.jobsW
			} else {
				l.jobsH = l.bodyH
			}
		} else {
			l.jobsH = l.bodyH
		}
	} else {
		l.leftW, l.jobsW, l.detailsW = l.width, l.width, l.width
		switch {
		case l.bodyH >= 16:
			// All three panes can be useful at this height.
			l.mode = bodyNarrowStack
			leftH := l.bodyH * 25 / 100
			if leftH < 5 {
				leftH = 5
			}
			jobsH := l.bodyH * 30 / 100
			if jobsH < 5 {
				jobsH = 5
			}
			detailsH := l.bodyH - leftH - jobsH
			if detailsH >= 5 {
				l.leftH, l.jobsH, l.detailsH = leftH, jobsH, detailsH
			} else {
				l.mode = bodyNarrowFocus
				l.leftH, l.jobsH = l.bodyH/2, l.bodyH-l.bodyH/2
			}
		case l.bodyH >= 10:
			// Two list panes: each gets enough room for at least one row.
			l.mode = bodyNarrowFocus
			l.leftH = l.bodyH / 2
			l.jobsH = l.bodyH - l.leftH
		case l.bodyH > 0:
			l.mode = bodyNarrowFocus
			if m.focused == PanelSubmissions {
				l.leftH = l.bodyH
			} else {
				l.jobsH = l.bodyH
			}
		}
	}
	return l
}

// listBudget converts a pane height into the number of item slots that fit.
// fixed is the number of non-item lines (title, column header); rowsPerItem
// is 2 for submissions (command + metadata) and 1 for jobs.
func listBudget(paneH, fixed, rowsPerItem int) int {
	if paneH < 3 {
		return 0
	}
	avail := paneH - 2 - fixed // borders
	if avail < 1 || rowsPerItem < 1 {
		return 0
	}
	return avail / rowsPerItem
}

// listBudgets returns how many rows fit in the submissions/jobs panes.
func (m Model) listBudgets() (subRows, jobRows int) {
	l := m.computeLayout()
	return listBudget(l.leftH, 1, 2), listBudget(l.jobsH, 2, 1)
}

func (m *Model) ensureVisible() {
	subRows, jobRows := m.listBudgets()
	m.subOff = clampWindow(m.subOff, m.selSub, len(m.submissions), subRows)
	m.jobOff = clampWindow(m.jobOff, m.selJob, len(m.visibleJobs()), jobRows)
}

func clampWindow(off, sel, total, budget int) int {
	if total <= 0 || budget < 1 {
		return 0
	}
	if budget > total {
		budget = total
	}
	if sel < 0 {
		sel = 0
	}
	if sel >= total {
		sel = total - 1
	}
	if off > total-budget {
		off = total - budget
	}
	if off < 0 {
		off = 0
	}
	if sel < off {
		return sel
	}
	if sel >= off+budget {
		return sel - budget + 1
	}
	return off
}

// windowRows slices [0,total) into the visible [start,end) plus a footer
// marker when truncated. The selection is always inside the window. When the
// budget is too small for both an item and a footer, the selected item is
// shown without a footer rather than overflowing the pane.
func windowRows(off, sel, total, budget int) (start, end int, footer string) {
	if budget < 1 || total <= 0 {
		return 0, 0, ""
	}
	if sel < 0 {
		sel = 0
	}
	if sel >= total {
		sel = total - 1
	}
	if total <= budget {
		return 0, total, ""
	}
	shown := budget - 1 // one row reserved for the footer
	if shown < 1 {
		return sel, sel + 1, ""
	}
	if off > total-shown {
		off = total - shown
	}
	if off < 0 {
		off = 0
	}
	if sel < off {
		off = sel
	}
	if sel >= off+shown {
		off = sel - shown + 1
	}
	end = off + shown
	above, below := off, total-end
	footer = fmt.Sprintf("… %d above · %d below …", above, below)
	return off, end, footer
}

// fitLines truncates every line to width and caps the result to height lines.
// It is ANSI-aware, so styled rows stay intact.
func fitLines(s string, width, height int) string {
	if width < 1 || height < 1 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = truncateANSI(lines[i], width)
	}
	return strings.Join(lines, "\n")
}

// truncateANSI shortens a single line without breaking escape sequences.
func truncateANSI(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "")
}

// fitScreen is the final safety net: no rendered frame may be wider or taller
// than the terminal. Lines are padded so popups can be composited over them.
func fitScreen(s string, width, height int) string {
	if width < 1 || height < 1 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	out := make([]string, height)
	for i := 0; i < height; i++ {
		line := ""
		if i < len(lines) {
			line = truncateANSI(lines[i], width)
		}
		if pad := width - ansi.StringWidth(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		out[i] = line
	}
	return strings.Join(out, "\n")
}

// renderPane draws one bordered pane at exactly w x h cells. Content is
// clipped before rendering, so neither long lines nor long lists can expand
// the box beyond its slot.
func renderPane(content string, w, h int, focused bool) string {
	if w < 2 || h < 2 {
		return ""
	}
	innerW, innerH := w-2, h-2
	content = fitLines(content, innerW, innerH)
	return paneStyle(focused).
		Width(innerW).
		Height(innerH).
		MaxWidth(w).
		MaxHeight(h).
		Render(content)
}

// renderPopup draws a bordered popup no larger than the terminal. It clips
// (rather than wraps) content so an oversized form/help text can never push
// the layout off screen.
func renderPopup(content string, w, h int) string {
	if w < 4 || h < 3 {
		return ""
	}
	innerW, innerH := w-2, h-2
	content = fitLines(content, innerW, innerH)
	return styleBorder.
		Width(innerW).
		Height(innerH).
		MaxWidth(w).
		MaxHeight(h).
		Render(content)
}

// overlayCenter places popup over a full-screen base frame.
func overlayCenter(base, popup string, width, height int) string {
	if popup == "" || width < 1 || height < 1 {
		return base
	}
	base = fitScreen(base, width, height)
	popLines := strings.Split(popup, "\n")
	baseLines := strings.Split(base, "\n")
	popH := len(popLines)
	popW := 0
	for _, ln := range popLines {
		if w := ansi.StringWidth(ln); w > popW {
			popW = w
		}
	}
	if popW < 1 || popH < 1 {
		return base
	}
	if popW > width {
		popW = width
	}
	if popH > height {
		popH = height
		popLines = popLines[:popH]
	}
	startX := (width - popW) / 2
	startY := (height - popH) / 2
	if startX < 0 {
		startX = 0
	}
	if startY < 0 {
		startY = 0
	}
	for i := 0; i < popH && startY+i < len(baseLines); i++ {
		baseLine := fitScreen(baseLines[startY+i], width, 1)
		left := ansi.Cut(baseLine, 0, startX)
		if pad := startX - ansi.StringWidth(left); pad > 0 {
			left += strings.Repeat(" ", pad)
		}
		popLine := popLines[i]
		if pad := popW - ansi.StringWidth(popLine); pad > 0 {
			popLine += strings.Repeat(" ", pad)
		}
		right := ansi.Cut(baseLine, startX+popW, width)
		baseLines[startY+i] = left + popLine + right
	}
	return strings.Join(baseLines, "\n")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func (m *Model) refreshLogTarget() (jobID, path string) {
	j := m.selectedJob()
	if j == nil {
		return "", ""
	}
	return j.ID, slurm.ResolveLogPath(m.cfg.LogDir, j.Name, j.ID, m.logWantErr)
}

// refreshLogPaths resolves both stream paths for the selected job.
func (m *Model) refreshLogPaths() (jobID, outPath, errPath string) {
	j := m.selectedJob()
	if j == nil {
		return "", "", ""
	}
	return j.ID,
		slurm.ResolveLogPath(m.cfg.LogDir, j.Name, j.ID, false),
		slurm.ResolveLogPath(m.cfg.LogDir, j.Name, j.ID, true)
}

// setLogContent updates the viewport, preserving scroll unless following.
func (m *Model) setLogContent(content string) {
	m.logView.SetContent(content)
	if m.logFollow {
		m.logView.GotoBottom()
	}
}

func jobStateColor(state string) lipgloss.Color {
	switch strings.ToUpper(state) {
	case "RUNNING", "R":
		return lipgloss.Color("42")
	case "PENDING", "PD":
		return lipgloss.Color("214")
	case "COMPLETED", "CD":
		return lipgloss.Color("39")
	case "FAILED", "F", "TIMEOUT", "TO", "OUT_OF_MEMORY", "OOM", "CANCELLED", "CA":
		return lipgloss.Color("196")
	case "SUSPENDED", "S":
		return lipgloss.Color(" Magenta")
	default:
		return lipgloss.Color("252")
	}
}

// Update -------------------------------------------------------------------

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.resizeInputs()
		m.resizeLogView()
		m.clampSelections()
		return m, nil

	case tickMsg:
		cmds := []tea.Cmd{m.tickCmd(), m.fetchJobsCmd()}
		// Expire stale "submitting" guard? No, submit completion clears it.
		return m, tea.Batch(cmds...)

	case logTickMsg:
		cmds := []tea.Cmd{m.logTickCmd()}
		if m.mode == ModeNormal && m.logPinned == "" {
			if id, outPath, errPath := m.refreshLogPaths(); id != "" && outPath != "" {
				// Only auto-tail when following or when the log pane is focused.
				if m.logFollow || m.focused == PanelLogs {
					m.logJobID = id
					cmds = append(cmds, m.fetchLogsCmd(id, outPath, errPath))
				}
			}
		}
		return m, tea.Batch(cmds...)

	case jobsMsg:
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("squeue: %v", msg.err), true)
		} else {
			prevSel := ""
			if j := m.selectedJob(); j != nil {
				prevSel = j.ID
			}
			m.jobs = msg.jobs
			sort.Slice(m.jobs, func(i, j int) bool { return m.jobs[i].ID < m.jobs[j].ID })
			m.clampSelections()
			// Restore selection by job id across refreshes.
			if prevSel != "" {
				for i, j := range m.visibleJobs() {
					if j.ID == prevSel {
						m.selJob = i
						break
					}
				}
			}
			m.ensureVisible()
			if j := m.selectedJob(); j != nil {
				key := j.ID
				if key != m.lastJobKey {
					m.lastJobKey = key
					return m, m.fetchScontrolCmd(key)
				}
			}
		}
		return m, nil

	case logsMsg:
		// Drop stale reads, unless they belong to a pinned history view
		// (finished job, no longer in squeue).
		pinned := m.logPinned != "" && msg.jobID == m.logPinned
		if !pinned {
			if cur := m.selectedJob(); cur == nil || cur.ID != msg.jobID {
				// Still show it if nothing selected? No — drop.
				return m, nil
			}
		}
		m.logJobID = msg.jobID
		m.logOutPath, m.logErrPath = msg.outPath, msg.errPath
		m.logOutLines, m.logErrLines = countLines(msg.out), countLines(msg.errOut)
		content, stream := pickLogStream(m.logWantErr, msg.out, msg.errOut)
		m.logStream = stream
		switch stream {
		case ".err":
			m.logPath = msg.errPath
		default:
			m.logPath = msg.outPath
		}
		if content == "" {
			// Both streams empty.
			if msg.outErr != nil || msg.errRead != nil {
				first := msg.outErr
				if first == nil {
					first = msg.errRead
				}
				content = fmt.Sprintf("Error reading logs:\n%s\n%s\n%v", msg.outPath, msg.errPath, first)
			} else {
				content = fmt.Sprintf("No output yet (stdout and stderr empty)\n%s\n%s\n(job may be pending or just started)",
					msg.outPath, msg.errPath)
			}
		}
		m.setLogContent(content)
		return m, nil

	case scontrolMsg:
		if cur := m.selectedJob(); cur != nil && cur.ID == msg.jobID {
			m.scontrolJobID = msg.jobID
			m.scontrolInfo = msg.info
		}
		return m, nil

	case submitDoneMsg:
		m.submitting = false
		m.logPinned, m.logPinnedOut, m.logPinnedErr = "", "", ""
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("sbatch failed: %v %s", msg.err, msg.raw), true)
			return m, nil
		}
		subs, err := m.store.List()
		if err == nil {
			m.submissions = subs
			m.selSub = 0
			m.ensureVisible()
		}
		m.mode = ModeNormal
		m.setStatus(fmt.Sprintf("Submitted batch job %s (%s)", msg.jobID, strings.TrimSpace(msg.raw)), false)
		return m, tea.Batch(m.fetchJobsCmd(), m.fetchLogsCmd(msg.jobID,
			slurm.ResolveLogPath(m.cfg.LogDir, msg.sub.JobName, msg.jobID, false),
			slurm.ResolveLogPath(m.cfg.LogDir, msg.sub.JobName, msg.jobID, true)))

	case cancelDoneMsg:
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("scancel %s failed: %v %s", msg.jobID, msg.err, msg.raw), true)
		} else {
			m.setStatus(fmt.Sprintf("Cancelled job %s", msg.jobID), false)
		}
		return m, m.fetchJobsCmd()
	}

	// Mode-specific key handling.
	switch m.mode {
	case ModeHelp:
		return m.updateHelp(msg)
	case ModeConfirm:
		return m.updateConfirm(msg)
	case ModeSubmit:
		return m.updateSubmit(msg)
	}

	// Normal mode (plus filter input capture).
	if m.filtering {
		return m.updateFiltering(msg)
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.updateNormalKey(msg)
	}

	// Viewport scrolling via mouse/keys falls through here when logs focused.
	if m.focused == PanelLogs {
		var cmd tea.Cmd
		m.logView, cmd = m.logView.Update(msg)
		return m, cmd
	}
	return m, nil
}

// resizeInputs keeps text fields inside their eventual popup/filter bars.
// These widths are display-only; the popup re-clamps them again on render.
func (m *Model) resizeInputs() {
	w := m.width
	if w <= 0 {
		return
	}
	inputW := clampInt(w-24, 8, 60)
	for i := range m.submitInputs {
		if i == fieldCPUs {
			m.submitInputs[i].Width = 8
		} else {
			m.submitInputs[i].Width = inputW
		}
	}
	m.filterInput.Width = clampInt(w-32, 8, 40)
}

func (m *Model) resizeLogView() {
	// Match viewLogs exactly: pane inner width, and inner height minus the
	// title row. Keeping the persistent viewport in sync with the rendered
	// one makes follow/scroll behaviour stable across font-size changes.
	l := m.computeLayout()
	m.logView.Width = max(l.width-2, 1)
	m.logView.Height = max(l.logH-3, 1)
}

// Normal-mode keys (vim-flavoured, lazygit-inspired).
func (m Model) updateNormalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "ctrl+c":
		return m, tea.Quit
	case "q":
		return m, tea.Quit
	case "?":
		m.mode = ModeHelp
		m.helpOff = 0
		return m, nil
	case "1":
		m.focused = PanelSubmissions
		return m, nil
	case "2":
		m.focused = PanelJobs
		return m, nil
	case "3":
		m.focused = PanelLogs
		return m, nil
	case "tab":
		m.focused = (m.focused + 1) % 3
		return m, nil
	case "shift+tab":
		m.focused = (m.focused + 2) % 3
		return m, nil
	case "h", "left":
		// h moves focus left; in log pane it does nothing (no horizontal nav).
		if m.focused == PanelJobs {
			m.focused = PanelSubmissions
		} else if m.focused == PanelLogs {
			m.focused = PanelJobs
		}
		return m, nil
	case "l", "right":
		if m.focused == PanelSubmissions {
			m.focused = PanelJobs
		} else if m.focused == PanelJobs {
			m.focused = PanelLogs
			m.logPinned, m.logPinnedOut, m.logPinnedErr = "", "", ""
			// Entering log pane loads current job's log immediately.
			if id, outPath, errPath := m.refreshLogPaths(); id != "" {
				m.logJobID = id
				return m, m.fetchLogsCmd(id, outPath, errPath)
			}
		}
		return m, nil
	case "j", "down":
		return m.move(1)
	case "k", "up":
		return m.move(-1)
	case "g":
		return m.moveToEdge(true)
	case "G":
		return m.moveToEdge(false)
	case "ctrl+d":
		return m.move(10)
	case "ctrl+u":
		return m.move(-10)
	case "s", "c", "n":
		m.openSubmit()
		return m, m.submitInputs[0].Focus()
	case "enter", "o":
		if m.focused == PanelSubmissions {
			// Jump to the first live job belonging to this submission,
			// or load its archived log when it already left squeue.
			if sub := m.selectedSubmission(); sub != nil && len(sub.JobIDs) > 0 {
				want := sub.JobIDs[0]
				for i, j := range m.visibleJobs() {
					if j.ID == want {
						m.selJob = i
						m.focused = PanelJobs
						m.lastJobKey = ""
						m.logPinned, m.logPinnedOut, m.logPinnedErr = "", "", ""
						return m, m.fetchScontrolCmd(want)
					}
				}
				return m.enterHistoryLog(sub, want)
			}
			return m, nil
		}
		// In jobs: jump to logs.
		m.focused = PanelLogs
		m.logPinned, m.logPinnedOut, m.logPinnedErr = "", "", ""
		if id, outPath, errPath := m.refreshLogPaths(); id != "" {
			m.logJobID = id
			return m, m.fetchLogsCmd(id, outPath, errPath)
		}
		return m, nil
	case "r":
		subs, err := m.store.List()
		if err != nil {
			m.setStatus(fmt.Sprintf("history: %v", err), true)
		} else {
			m.submissions = subs
		}
		m.setStatus("Refreshing…", false)
		cmds := []tea.Cmd{m.fetchJobsCmd()}
		if m.logPinned != "" {
			// Keep showing the archived log, just re-read it.
			cmds = append(cmds, m.fetchLogsCmd(m.logPinned, m.logPinnedOut, m.logPinnedErr))
		} else if id, outPath, errPath := m.refreshLogPaths(); id != "" {
			cmds = append(cmds, m.fetchLogsCmd(id, outPath, errPath))
			cmds = append(cmds, m.fetchScontrolCmd(id))
		}
		return m, tea.Batch(cmds...)
	case "x":
		if j := m.selectedJob(); j != nil {
			m.confirmJobID = j.ID
			m.mode = ModeConfirm
		}
		return m, nil
	case "e":
		m.logWantErr = !m.logWantErr
		if m.logPinned != "" {
			what := "stdout (.out) — stderr auto-shows when empty"
			if m.logWantErr {
				what = "stderr (.err) — stdout auto-shows when empty"
			}
			m.setStatus("Log stream: "+what, false)
			return m, m.fetchLogsCmd(m.logPinned, m.logPinnedOut, m.logPinnedErr)
		}
		if id, outPath, errPath := m.refreshLogPaths(); id != "" {
			what := "stdout (.out) — stderr auto-shows when empty"
			if m.logWantErr {
				what = "stderr (.err) — stdout auto-shows when empty"
			}
			m.setStatus("Log stream: "+what, false)
			m.logJobID = id
			return m, m.fetchLogsCmd(id, outPath, errPath)
		}
		return m, nil
	case "f":
		m.logFollow = !m.logFollow
		if m.logFollow {
			m.logView.GotoBottom()
			m.setStatus("Log follow: on", false)
		} else {
			m.setStatus("Log follow: off", false)
		}
		return m, nil
	case "d":
		if j := m.selectedJob(); j != nil {
			return m, m.fetchScontrolCmd(j.ID)
		}
		return m, nil
	case "y":
		// Yank: surface useful strings in the status bar for copy/paste.
		if m.focused == PanelSubmissions {
			if s := m.selectedSubmission(); s != nil {
				m.setStatus("cmd: "+s.Command, false)
			}
		} else if j := m.selectedJob(); j != nil {
			m.setStatus(fmt.Sprintf("job %s log: %s", j.ID, slurm.ResolveLogPath(m.cfg.LogDir, j.Name, j.ID, m.logWantErr)), false)
		}
		return m, nil
	case "/":
		m.filtering = true
		m.filterInput.SetValue(m.filter)
		m.filterInput.Focus()
		return m, nil
	case "esc":
		if m.filter != "" {
			m.filter = ""
			m.selJob = 0
		}
		return m, nil
	}

	// Log-pane scrolling when focused (j/k handled above via move(); here
	// we forward remaining keys like pgup/pgdown to the viewport).
	if m.focused == PanelLogs {
		var cmd tea.Cmd
		m.logView, cmd = m.logView.Update(msg)
		// Any manual scroll disables follow; `f` re-enables.
		if k == "j" || k == "k" || k == "up" || k == "down" || k == "pgup" || k == "pgdown" {
			_ = k
		}
		return m, cmd
	}
	return m, nil
}

func (m Model) move(delta int) (tea.Model, tea.Cmd) {
	switch m.focused {
	case PanelSubmissions:
		m.selSub += delta
		m.clampSelections()
	case PanelJobs:
		m.selJob += delta
		m.clampSelections()
		if j := m.selectedJob(); j != nil && j.ID != m.lastJobKey {
			m.lastJobKey = j.ID
			cmds := []tea.Cmd{m.fetchScontrolCmd(j.ID)}
			if m.logPinned != "" {
				// Back to a live job: unpin the archived view.
				m.logPinned, m.logPinnedOut, m.logPinnedErr = "", "", ""
				if _, outP, errP := m.refreshLogPaths(); outP != "" {
					m.logJobID = j.ID
					cmds = append(cmds, m.fetchLogsCmd(j.ID, outP, errP))
				}
			}
			return m, tea.Batch(cmds...)
		}
	case PanelLogs:
		if delta > 0 {
			m.logView.LineDown(delta)
		} else {
			m.logView.LineUp(-delta)
		}
		// Manual scroll pauses follow so output doesn't jump.
		if !m.logView.AtBottom() {
			m.logFollow = false
		}
	}
	return m, nil
}

func (m Model) moveToEdge(top bool) (tea.Model, tea.Cmd) {
	switch m.focused {
	case PanelSubmissions:
		if top {
			m.selSub = 0
		} else if len(m.submissions) > 0 {
			m.selSub = len(m.submissions) - 1
		}
	case PanelJobs:
		if top {
			m.selJob = 0
		} else if n := len(m.visibleJobs()); n > 0 {
			m.selJob = n - 1
		}
		m.clampSelections()
		if j := m.selectedJob(); j != nil && j.ID != m.lastJobKey {
			m.lastJobKey = j.ID
			cmds := []tea.Cmd{m.fetchScontrolCmd(j.ID)}
			if m.logPinned != "" {
				m.logPinned, m.logPinnedOut, m.logPinnedErr = "", "", ""
				if _, outP, errP := m.refreshLogPaths(); outP != "" {
					m.logJobID = j.ID
					cmds = append(cmds, m.fetchLogsCmd(j.ID, outP, errP))
				}
			}
			return m, tea.Batch(cmds...)
		}
	case PanelLogs:
		if top {
			m.logView.GotoTop()
			m.logFollow = false
		} else {
			m.logView.GotoBottom()
			m.logFollow = true
		}
	}
	m.clampSelections() // keep scroll offsets behind g/G jumps
	return m, nil
}

// enterHistoryLog pins the archived log of a finished job (in history but
// no longer in squeue) into the log pane.
func (m Model) enterHistoryLog(sub *history.Submission, jobID string) (tea.Model, tea.Cmd) {
	out := slurm.ResolveLogPath(m.cfg.LogDir, sub.JobName, jobID, false)
	errP := slurm.ResolveLogPath(m.cfg.LogDir, sub.JobName, jobID, true)
	if !fileExists(out) && !fileExists(errP) {
		m.setStatus(fmt.Sprintf("no log file for finished job %s (looked for %s)", jobID, out), true)
		return m, nil
	}
	m.logPinned, m.logPinnedOut, m.logPinnedErr = jobID, out, errP
	m.logJobID = jobID
	m.focused = PanelLogs
	return m, m.fetchLogsCmd(jobID, out, errP)
}

func (m *Model) openSubmit() {
	m.mode = ModeSubmit
	m.submitFocus = 0
	m.resetComp()
	m.submitInputs = makeSubmitInputs(m.cfg)
	m.resizeInputs()
	// Prefill command when resubmitting from a history entry.
	if sub := m.selectedSubmission(); sub != nil && strings.TrimSpace(sub.Command) != "" {
		m.submitInputs[fieldCommand].SetValue(sub.Command)
		m.submitInputs[fieldJobName].SetValue(sub.JobName)
		m.submitInputs[fieldCPUs].SetValue(strconv.Itoa(slurm.NormalizeCPUs(sub.CPUs)))
	}
	for i := range m.submitInputs {
		if i == 0 {
			m.submitInputs[i].Focus()
		} else {
			m.submitInputs[i].Blur()
		}
	}
}

// Submit popup --------------------------------------------------------------

var submitLabels = []string{"Command", "Job name", "Partition", "CPUs", "Time"}

func (m Model) updateSubmit(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		k := msg.String()
		switch k {
		case "esc":
			m.mode = ModeNormal
			for i := range m.submitInputs {
				m.submitInputs[i].Blur()
			}
			return m, nil
		case "ctrl+s":
			return m.doSubmit()
		case "tab":
			if m.submitFocus == fieldCommand {
				// Tab completes file names in the Command field;
				// use enter/down to advance to the next field.
				m.doComplete()
				return m, nil
			}
			m.submitAdvance(1)
			return m, m.focusCmd()
		case "shift+tab":
			m.submitAdvance(-1)
			return m, m.focusCmd()
		case "up":
			m.submitAdvance(-1)
			return m, m.focusCmd()
		case "down":
			m.submitAdvance(1)
			return m, m.focusCmd()
		case "enter":
			if m.submitFocus == len(m.submitInputs) {
				return m.doSubmit()
			}
			m.submitAdvance(1)
			return m, m.focusCmd()
		}
	}
	// Route typing to the focused input.
	if m.submitFocus < len(m.submitInputs) {
		// CPUs is a stepped 8–192 selector, not free text:
		// space/h/l cycle it, everything else is swallowed.
		if m.submitFocus == fieldCPUs {
			if km, ok := msg.(tea.KeyMsg); ok {
				switch km.String() {
				case "space", "l", "right", "+", "=":
					m.cycleCPUs(1)
					return m, nil
				case "h", "left", "-", "_":
					m.cycleCPUs(-1)
					return m, nil
				default:
					return m, nil
				}
			}
		}
		var cmd tea.Cmd
		before := m.submitInputs[m.submitFocus].Value()
		m.submitInputs[m.submitFocus], cmd = m.submitInputs[m.submitFocus].Update(msg)
		// Manual edits invalidate a pending completion cycle.
		if m.submitFocus == fieldCommand && m.submitInputs[0].Value() != before {
			m.resetComp()
		}
		return m, cmd
	}
	return m, nil
}

// cycleCPUs steps the CPUs selector through slurm.AllowedCPUs.
func (m *Model) cycleCPUs(d int) {
	n := len(slurm.AllowedCPUs)
	cur, _ := strconv.Atoi(strings.TrimSpace(m.submitInputs[fieldCPUs].Value()))
	idx := (slurm.CPUIndex(cur) + d + n) % n
	m.submitInputs[fieldCPUs].SetValue(strconv.Itoa(slurm.AllowedCPUs[idx]))
}

func (m *Model) submitAdvance(d int) {
	n := len(m.submitInputs) + 1 // + submit button
	m.submitFocus = (m.submitFocus + d + n) % n
	m.resetComp()
}

func (m Model) focusCmd() tea.Cmd {
	var cmds []tea.Cmd
	for i := range m.submitInputs {
		// The CPUs row is a stepped selector (space/h/l), not free
		// text, so it never takes keyboard focus.
		if i == m.submitFocus && i != fieldCPUs {
			cmds = append(cmds, m.submitInputs[i].Focus())
		} else {
			m.submitInputs[i].Blur()
		}
	}
	return tea.Batch(cmds...)
}

func (m Model) doSubmit() (tea.Model, tea.Cmd) {
	cmdStr := strings.TrimSpace(m.submitInputs[fieldCommand].Value())
	if cmdStr == "" {
		m.setStatus("Command is empty — type the shell command to run.", true)
		return m, nil
	}
	cpus, err := strconv.Atoi(strings.TrimSpace(m.submitInputs[fieldCPUs].Value()))
	if err != nil {
		cpus = m.cfg.DefaultCPUs
	}
	cpus = slurm.NormalizeCPUs(cpus)
	wd, _ := os.Getwd()
	cmdEff := slurm.EffectiveCommand(wd, cmdStr)
	kind := ""
	jobName := slurm.SafeJobName(strings.TrimSpace(m.submitInputs[fieldJobName].Value()))
	if _, parsed, ok := slurm.DetectScript(wd, cmdStr); ok {
		// Direct script submission: the script's own resource directives
		// apply, so the form's CPUs/Time/Partition must not leak into
		// history (nor into sbatch — see SubmitJob).
		kind = "script"
		jobName = parsed
	}
	sub := history.Submission{
		Timestamp: time.Now(),
		Command:   cmdEff,
		JobName:   jobName,
		Kind:      kind,
		WorkDir:   wd,
	}
	if kind == "" {
		sub.Partition = strings.TrimSpace(m.submitInputs[fieldPartition].Value())
		sub.CPUs = cpus
		sub.Time = strings.TrimSpace(m.submitInputs[fieldTime].Value())
	}
	if sub.JobName == "" {
		sub.JobName = m.cfg.DefaultJobName
	}
	if kind == "" && sub.Time == "" {
		sub.Time = m.cfg.DefaultTime
	}
	o := slurm.SubmitOptions{
		Command:   sub.Command,
		JobName:   sub.JobName,
		Partition: sub.Partition,
		CPUs:      sub.CPUs,
		Time:      sub.Time,
		LogDir:    m.cfg.LogDir,
		WorkDir:   sub.WorkDir,
	}
	m.submitting = true
	m.setStatus("Submitting via sbatch…", false)
	return m, m.submitCmd(o, sub)
}

// Confirm popup ---------------------------------------------------------------

func (m Model) updateConfirm(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			id := m.confirmJobID
			m.mode = ModeNormal
			m.confirmJobID = ""
			m.setStatus("Cancelling job "+id+"…", false)
			return m, m.cancelCmd(id)
		case "n", "N", "esc", "q":
			m.mode = ModeNormal
			m.confirmJobID = ""
			return m, nil
		}
	}
	return m, nil
}

// Help popup ------------------------------------------------------------------

func (m Model) updateHelp(msg tea.Msg) (tea.Model, tea.Cmd) {
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "esc", "q", "?", "enter":
			m.mode = ModeNormal
			return m, nil
		case "j", "down":
			m.helpOff++
		case "k", "up":
			m.helpOff--
		case "ctrl+d", "pgdown":
			m.helpOff += 8
		case "ctrl+u", "pgup":
			m.helpOff -= 8
		case "g", "home":
			m.helpOff = 0
		case "G", "end":
			m.helpOff = m.maxHelpOffset()
		}
		m.helpOff = clampInt(m.helpOff, 0, m.maxHelpOffset())
	}
	return m, nil
}

// maxHelpOffset is the largest scroll offset that still fills the popup.
func (m Model) maxHelpOffset() int {
	innerH := max(m.height-2, 1)
	lines := m.helpContentLines(max(m.popupWidth()-2, 1))
	return max(0, len(lines)-innerH)
}

// Filter input -----------------------------------------------------------------

func (m Model) updateFiltering(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.filtering = false
			m.filterInput.Blur()
			return m, nil
		case "enter":
			m.filter = strings.TrimSpace(m.filterInput.Value())
			m.filtering = false
			m.filterInput.Blur()
			m.selJob = 0
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	// Live filter as you type.
	m.filter = strings.TrimSpace(m.filterInput.Value())
	m.clampSelections()
	return m, cmd
}

// View ------------------------------------------------------------------------

var (
	styleTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleSel     = lipgloss.NewStyle().Background(lipgloss.Color("238")).Bold(true)
	styleBorder  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))
	styleFocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("42"))
)

func paneStyle(focused bool) lipgloss.Style {
	if focused {
		return styleFocused
	}
	return styleBorder
}

func (m Model) View() string {
	if !m.ready {
		return "Loading…"
	}
	l := m.computeLayout()
	if l.tooSmall {
		base := fitScreen(m.viewTooSmall(), m.width, m.height)
		return m.applyPopup(base, l)
	}

	parts := []string{m.viewHeader()}
	if l.bodyH > 0 {
		parts = append(parts, m.viewBody(l))
	}
	if l.showLog {
		parts = append(parts, renderPane(m.viewLogs(l), l.width, l.logH, m.focused == PanelLogs))
	}
	parts = append(parts, m.viewStatus(l))

	// fitScreen is deliberately a final guard: even a future layout bug
	// cannot emit a frame larger than the terminal.
	out := fitScreen(strings.Join(parts, "\n"), l.width, l.height)
	return m.applyPopup(out, l)
}

// viewBody lays out the submissions/jobs/details panes according to the
// responsive body mode. Every pane gets an exact slot; joining them cannot
// overflow because the widths and heights sum to the content area.
func (m Model) viewBody(l layout) string {
	subFocused := m.focused == PanelSubmissions
	jobFocused := m.focused == PanelJobs

	switch l.mode {
	case bodyWide:
		pieces := []string{
			renderPane(m.viewSubmissions(), l.leftW, l.leftH, subFocused),
			renderPane(m.viewJobs(), l.jobsW, l.jobsH, jobFocused),
		}
		if l.detailsW > 0 && l.detailsH > 0 {
			pieces = append(pieces, renderPane(m.viewDetails(), l.detailsW, l.detailsH, false))
		}
		return lipgloss.JoinHorizontal(lipgloss.Top, pieces...)

	case bodyMedium:
		left := renderPane(m.viewSubmissions(), l.leftW, l.leftH, subFocused)
		right := make([]string, 0, 2)
		if l.jobsH > 0 {
			right = append(right, renderPane(m.viewJobs(), l.jobsW, l.jobsH, jobFocused))
		}
		if l.detailsH > 0 {
			right = append(right, renderPane(m.viewDetails(), l.detailsW, l.detailsH, false))
		}
		return lipgloss.JoinHorizontal(lipgloss.Top, left,
			lipgloss.JoinVertical(lipgloss.Left, right...))

	default: // narrow stacked / focused
		pieces := make([]string, 0, 3)
		if l.leftH > 0 {
			pieces = append(pieces, renderPane(m.viewSubmissions(), l.leftW, l.leftH, subFocused))
		}
		if l.jobsH > 0 {
			pieces = append(pieces, renderPane(m.viewJobs(), l.jobsW, l.jobsH, jobFocused))
		}
		if l.detailsH > 0 {
			pieces = append(pieces, renderPane(m.viewDetails(), l.detailsW, l.detailsH, false))
		}
		return lipgloss.JoinVertical(lipgloss.Left, pieces...)
	}
}

// applyPopup overlays the active modal on the already-fitted frame.
func (m Model) applyPopup(base string, l layout) string {
	var popup string
	switch m.mode {
	case ModeSubmit:
		popup = m.viewSubmitPopup()
	case ModeConfirm:
		popup = m.viewConfirmPopup()
	case ModeHelp:
		popup = m.viewHelpPopup()
	}
	if popup == "" {
		return base
	}
	return overlayCenter(base, popup, l.width, l.height)
}

func (m Model) viewTooSmall() string {
	msg := fmt.Sprintf("Terminal too small (%dx%d).\nResize the window or reduce the font size.\nNeed at least %dx%d.",
		m.width, m.height, minLayoutWidth, minLayoutHeight)
	if m.width < 2 || m.height < 1 {
		return ""
	}
	return fitLines(msg, m.width, m.height)
}

func (m Model) viewHeader() string {
	nRun, nPend := 0, 0
	for _, j := range m.jobs {
		switch strings.ToUpper(j.State) {
		case "RUNNING":
			nRun++
		case "PENDING":
			nPend++
		}
	}
	f := ""
	if m.filter != "" {
		f = fmt.Sprintf(" filter=%q (%d/%d)", m.filter, len(m.visibleJobs()), len(m.jobs))
	}
	var stats string
	switch {
	case m.width >= 100:
		stats = fmt.Sprintf("jobs=%d (R:%d PD:%d) hist=%d poll=%ds userOnly=%v%s  •  ? help",
			len(m.jobs), nRun, nPend, len(m.submissions), m.cfg.PollSecs, m.cfg.UserOnly, f)
	case m.width >= 60:
		stats = fmt.Sprintf("jobs=%d R:%d PD:%d hist=%d%s  •  ? help",
			len(m.jobs), nRun, nPend, len(m.submissions), f)
	case m.width >= 32:
		stats = fmt.Sprintf("jobs=%d R:%d PD:%d%s", len(m.jobs), nRun, nPend, f)
	default:
		stats = fmt.Sprintf("%d jobs", len(m.jobs))
	}
	return truncateANSI(fmt.Sprintf("%s  %s", styleTitle.Render("lazyslurm"), styleDim.Render(stats)), m.width)
}

func (m Model) viewSubmissions() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render(fmt.Sprintf("Submissions (%d)", len(m.submissions))) + "\n")
	if len(m.submissions) == 0 {
		b.WriteString(styleDim.Render("No submissions yet.\nPress s to run your first command."))
		return b.String()
	}
	subRows, _ := m.listBudgets()
	start, end, footer := windowRows(m.subOff, m.selSub, len(m.submissions), subRows)
	for i := start; i < end; i++ {
		s := m.submissions[i]
		mark := "  "
		if i == m.selSub && m.focused == PanelSubmissions {
			mark = "> "
		}
		line := fmt.Sprintf("%s%s [%s] %s", mark, s.Timestamp.Format("01-02 15:04"), strings.Join(s.JobIDs, ","), truncate(s.Command, 60))
		if i == m.selSub {
			line = styleSel.Render(line)
		}
		b.WriteString(line + "\n")
		sub2 := fmt.Sprintf("    name=%s cpus=%d time=%s dir=%s", s.JobName, s.CPUs, s.Time, s.WorkDir)
		if s.Kind == "script" {
			sub2 = fmt.Sprintf("    sbatch script %s dir=%s", s.JobName, s.WorkDir)
		}
		b.WriteString(styleDim.Render(truncate(sub2, 100)) + "\n")
	}
	if footer != "" {
		b.WriteString(styleDim.Render(footer))
	}
	return b.String()
}

func (m Model) viewJobs() string {
	v := m.visibleJobs()
	var b strings.Builder
	b.WriteString(styleTitle.Render(fmt.Sprintf("Jobs (%d)", len(v))) + "\n")
	b.WriteString(styleDim.Render(fmt.Sprintf("%-10s %-16s %-4s %-9s %-8s %-5s %-8s %-8s %s",
		"JOBID", "NAME", "ST", "STATE", "TIME", "CPUS", "MEM", "PART", "NODE")) + "\n")
	if len(v) == 0 {
		b.WriteString(styleDim.Render("(empty — squeue returned no jobs)"))
		return b.String()
	}
	_, jobRows := m.listBudgets()
	start, end, footer := windowRows(m.jobOff, m.selJob, len(v), jobRows)
	for i := start; i < end; i++ {
		j := v[i]
		st := lipgloss.NewStyle().Foreground(jobStateColor(j.State)).Render(fmt.Sprintf("%-4s", j.StateShort))
		line := fmt.Sprintf("%-10s %-16s %s %-9s %-8s %-5s %-8s %-8s %s",
			truncate(j.ID, 10), truncate(j.Name, 16), st,
			truncate(j.State, 9), truncate(j.TimeUsed, 8), truncate(j.CPUs, 5),
			truncate(j.Mem, 8), truncate(j.Partition, 8), truncate(j.NodeList, 12))
		if i == m.selJob {
			line = styleSel.Render(fmt.Sprintf("  %s", stripANSI(line)))
			if m.focused == PanelJobs {
				line = "> " + strings.TrimPrefix(line, "  ")
			}
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	if footer != "" {
		b.WriteString(styleDim.Render(footer))
	}
	return b.String()
}

func (m Model) viewDetails() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Details") + "\n")
	if m.logPinned != "" {
		// Archived view: describe the finished job from history.
		for _, s := range m.submissions {
			for _, id := range s.JobIDs {
				if id != m.logPinned {
					continue
				}
				rows := [][2]string{
					{"ID", m.logPinned + " (finished)"},
					{"Name", s.JobName},
					{"Ran", s.Timestamp.Format("01-02 15:04")},
					{"Dir", s.WorkDir},
				}
				for _, r := range rows {
					b.WriteString(fmt.Sprintf("%-8s %s\n", r[0]+":", truncate(r[1], 28)))
				}
				b.WriteString(styleDim.Render("— cmd —") + "\n")
				b.WriteString(truncate(s.Command, 120) + "\n")
				return b.String()
			}
		}
	}
	j := m.selectedJob()
	if j == nil {
		b.WriteString(styleDim.Render("No job selected."))
		return b.String()
	}
	rows := [][2]string{
		{"ID", j.ID},
		{"Name", j.Name},
		{"User", j.User},
		{"State", j.State + " (" + j.StateShort + ")"},
		{"Time", j.TimeUsed + " / " + j.TimeLimit},
		{"CPUs", j.CPUs},
		{"Mem", j.Mem},
		{"Nodes", j.Nodes},
		{"Partition", j.Partition},
		{"Nodelist", j.NodeList},
		{"QOS", j.QOS},
		{"Start", j.StartTime},
		{"Submit", j.SubmitTime},
	}
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("%-8s %s\n", r[0]+":", truncate(r[1], 28)))
	}
	if m.scontrolJobID == j.ID && len(m.scontrolInfo) > 0 {
		b.WriteString(styleDim.Render("— scontrol —") + "\n")
		keys := []string{"JobState", "Reason", "NumNodes", "NumCPUs", "MinMemoryNode", "MinMemoryNode", "Partition", "WorkDir", "StdOut", "StdErr"}
		seen := map[string]bool{}
		for _, k := range keys {
			if v, ok := m.scontrolInfo[k]; ok && v != "" {
				b.WriteString(styleDim.Render(fmt.Sprintf("%s=%s\n", k, truncate(v, 34))))
				seen[k] = true
			}
		}
		_ = seen
	}
	// Linked submission command.
	for _, s := range m.submissions {
		for _, id := range s.JobIDs {
			if id == j.ID {
				b.WriteString(styleDim.Render("— cmd —") + "\n")
				b.WriteString(truncate(s.Command, 120) + "\n")
			}
		}
	}
	_, logPath := m.refreshLogTarget()
	b.WriteString(styleDim.Render("log: " + truncate(logPath, 60)))
	return b.String()
}

func (m Model) viewLogs(l layout) string {
	var b strings.Builder
	title := "Logs"
	if m.logJobID != "" {
		showing := "…"
		if m.logStream != "" {
			showing = "showing stderr"
			if m.logStream == ".out" {
				showing = "showing stdout"
			}
		}
		follow := "follow:off"
		if m.logFollow {
			follow = "follow:on"
		}
		if l.width >= 80 {
			title = fmt.Sprintf("Logs — job %s · %s (out:%d lines err:%d lines) · %s · e toggles",
				m.logJobID, showing, m.logOutLines, m.logErrLines, follow)
		} else {
			title = fmt.Sprintf("Logs — %s · %s · %s", m.logJobID, showing, follow)
		}
	}
	b.WriteString(styleTitle.Render(title) + "\n")
	// The viewport must use the pane's real inner width/height. Using a
	// guessed full-screen width here was what let long log lines wrap
	// outside the box on narrow terminals or with a larger font.
	m.logView.Width = max(l.width-2, 1)
	m.logView.Height = max(l.logH-3, 1) // borders + title row
	if m.logFollow {
		m.logView.GotoBottom()
	} else if maxOff := m.logView.TotalLineCount() - m.logView.Height; maxOff >= 0 && m.logView.YOffset > maxOff {
		m.logView.YOffset = maxOff
	}
	b.WriteString(m.logView.View())
	return b.String()
}

func (m Model) viewStatus(l layout) string {
	lines := make([]string, 0, l.statusH)

	if m.filtering {
		lines = append(lines, truncateANSI("filter (enter=apply, esc=cancel): "+m.filterInput.View(), l.width))
	}
	if m.statusMsg != "" && time.Since(m.statusAt) < 12*time.Second {
		style := styleOK
		if m.statusErr {
			style = styleErr
		}
		lines = append(lines, truncateANSI(style.Render(m.statusMsg), l.width))
	}
	for _, help := range m.helpLines(l.width) {
		if len(lines) >= l.statusH {
			break
		}
		lines = append(lines, help)
	}
	if len(lines) > l.statusH {
		lines = lines[:l.statusH]
	}
	return strings.Join(lines, "\n")
}

// helpLines returns the compact key hints for the status bar. Narrow
// terminals get a shorter variant instead of a line that has to be cut.
func (m Model) helpLines(width int) []string {
	switch {
	case width >= 110:
		return []string{
			styleDim.Render("h/l panes • j/k move • 1/2/3 focus • s submit • enter logs • e out/err • f follow • x cancel • / filter • y yank • r refresh • ? help • q quit"),
			styleDim.Render("g/G top/bottom • ctrl-d/u page • tab/shift+tab cycle • d details • esc clear filter"),
		}
	case width >= 60:
		return []string{
			styleDim.Render("h/l panes • j/k move • s submit • enter logs • e out/err • x cancel • / filter • ? help • q quit"),
			styleDim.Render("g/G ends • ctrl-d/u page • tab cycle panes • f follow • r refresh"),
		}
	case width >= 32:
		return []string{
			styleDim.Render("j/k move • s submit • enter logs • / filter • ? help • q quit"),
			styleDim.Render("h/l panes • g/G ends • x cancel • f follow • r refresh"),
		}
	default:
		return []string{styleDim.Render("? help • q quit")}
	}
}

// popupWidth is the preferred popup width, always within the terminal.
func (m Model) popupWidth() int {
	if m.width < 4 {
		return 0
	}
	return clampInt(m.width, 4, 100)
}

func (m Model) viewSubmitPopup() string {
	w := m.popupWidth()
	if w == 0 || m.height < 3 || len(m.submitInputs) < len(submitLabels) {
		return ""
	}
	innerW := w - 2
	inputW := clampInt(innerW-14, 6, 60)
	inputs := append([]textinput.Model(nil), m.submitInputs...)
	for i := range inputs {
		if i == fieldCPUs {
			inputs[i].Width = 8
		} else {
			inputs[i].Width = inputW
		}
	}

	var b strings.Builder
	if innerW >= 70 {
		b.WriteString(styleTitle.Render("Submit — sbatch wrapper (tab=complete, tab/enter/down=next, esc=cancel, ctrl+s=submit)") + "\n")
	} else {
		b.WriteString(styleTitle.Render("Submit (tab/enter=next, esc=cancel, ctrl+s=submit)") + "\n")
	}
	for i, label := range submitLabels {
		cursor := "  "
		if i == m.submitFocus && m.mode == ModeSubmit {
			cursor = "> "
		}
		row := fmt.Sprintf("%s%-9s %s", cursor, label+":", inputs[i].View())
		if i == fieldCPUs && innerW >= 60 {
			row += "  " + styleDim.Render("(space/h/l cycle: 8–192 step 8)")
		}
		b.WriteString(row + "\n")
	}
	btn := "[ Submit ]"
	if m.submitFocus == len(m.submitInputs) {
		btn = styleSel.Render(">[ Submit ]")
	}
	b.WriteString("\n" + btn)
	if m.submitting {
		b.WriteString("  " + styleDim.Render("submitting…"))
	} else if innerW >= 60 {
		b.WriteString("  " + styleDim.Render(fmt.Sprintf("logs → %s/<name>-<jobid>.out|.err", m.cfg.LogDir)))
	}

	lines := strings.Count(b.String(), "\n") + 1
	h := clampInt(m.height, 3, lines+2)
	return renderPopup(b.String(), w, h)
}

func (m Model) viewConfirmPopup() string {
	pw := m.popupWidth()
	if pw < 4 || m.height < 3 {
		return ""
	}
	content := fmt.Sprintf("Cancel job %s?  (y=yes, n=no)", m.confirmJobID)
	w := clampInt(ansi.StringWidth(content)+4, 4, pw)
	return renderPopup(content, w, 3)
}

const helpText = `lazyslurm — lazygit-style slurm frontend

 PANES            focus with h/l, tab, or 1/2/3
  1 Submissions   saved shell commands + their job ids (history file)
  2 Jobs          live squeue: state, time, cpus, mem, partition
  3 Logs          tail of the job's .out/.err, follows live

 NORMAL KEYS (vim)
  j/k, ctrl-d/u  move / half-page      g/G top/bottom
  s/c/n          new submission (sbatch wrapper)
  enter/o        submissions→live job, or archived log if finished
                 jobs→logs
  x              cancel selected job (scancel, with confirm)
  e              toggle stdout/stderr log
  f              toggle log follow
  y              yank: show command (submissions) or log path (jobs)
  / + enter/esc  filter jobs          r refresh
  d              refresh scontrol details
  ?              this help            q quit
  j/k, ctrl-d/u  scroll this help      esc closes

 SUBMIT FORM
  Command is the shell line to run on the cluster. The job runs in the
  directory where you launched the TUI (injected as #SBATCH --chdir).
  A lone script path containing #SBATCH directives (benzene_au.sh, or
  "sbatch benzene_au.sh") is submitted directly with sbatch: only
  --job-name/--output/--error are overridden, the script's own resource
  directives (cpus, ntasks, mail, …) apply as written.
  Tab in the Command field completes file names from that directory
  (./script.sh, sub/dir/, ~/…, /abs/…); executables complete with a ./
  prefix so they run directly. Repeated Tab cycles matches, enter/down
  moves to the next field.
  CPUs steps through 8 to 192 in steps of 8 with space/h/l (memory
  is sized by the cluster from the CPU count, so there is no mem setting).
  The app injects --output/--error into <logdir>/<name>-<jobid>.out|.err
  so logs are always viewable here. CPUs/Time/Partition become
  #SBATCH directives. Every submit is appended to the history file.

 esc or ? closes this help.`

// helpContentLines wraps the help text to the popup's inner width. This keeps
// long paragraphs readable on narrow terminals instead of being cut off.
func (m Model) helpContentLines(width int) []string {
	if width < 4 {
		return nil
	}
	wrapped := lipgloss.NewStyle().Width(width).Render(helpText)
	lines := strings.Split(wrapped, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}

// viewHelpPopup renders a scrollable, width-wrapped help overlay.
func (m Model) viewHelpPopup() string {
	w := m.popupWidth()
	if w == 0 || m.height < 3 {
		return ""
	}
	lines := m.helpContentLines(w - 2)
	if len(lines) == 0 {
		return ""
	}
	innerH := m.height - 2
	if innerH < 1 {
		return ""
	}
	off := clampInt(m.helpOff, 0, max(0, len(lines)-innerH))
	end := min(len(lines), off+innerH)
	visible := lines[off:end]
	h := clampInt(len(visible)+2, 3, m.height)
	return renderPopup(strings.Join(visible, "\n"), w, h)
}

// utils ---------------------------------------------------------------------

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}

// stripANSI removes lipgloss color codes so selected-row rendering stays clean.
func stripANSI(s string) string {
	// lipgloss styles used here only wrap short state cells; a cheap
	// ESC-stripper is enough.
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		if !inEsc && s[i] == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') {
				inEsc = false
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
