package slurm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Job mirrors one squeue row plus the fields we care about.
type Job struct {
	ID         string
	Name       string
	User       string
	StateShort string // single-letter %t
	State      string // long %T
	TimeUsed   string
	TimeLimit  string
	Nodes      string
	CPUs       string
	Mem        string
	Partition  string
	NodeList   string
	QOS        string
	StartTime  string // %S actual/expected start
	SubmitTime string // %V submission time
}

// SubmitOptions describes one sbatch submission. Memory is intentionally
// absent: the cluster sizes memory from the CPU count.
type SubmitOptions struct {
	Command   string
	JobName   string
	Partition string
	CPUs      int
	Time      string
	LogDir    string
	// WorkDir becomes #SBATCH --chdir so the job runs where the TUI was
	// launched. Empty = current working directory at submit time.
	WorkDir string
}

// AllowedCPUs are the only CPU counts the cluster accepts: 8 to 192.
var AllowedCPUs = func() []int {
	out := make([]int, 0, 24)
	for n := 8; n <= 192; n += 8 {
		out = append(out, n)
	}
	return out
}()

// NormalizeCPUs snaps n to the nearest allowed count (8 on garbage).
func NormalizeCPUs(n int) int {
	best := AllowedCPUs[0]
	for _, a := range AllowedCPUs[1:] {
		da, db := abs(a-n), abs(best-n)
		if da < db {
			best = a
		}
	}
	return best
}

// CPUIndex returns the position of NormalizeCPUs(n) in AllowedCPUs.
func CPUIndex(n int) int {
	n = NormalizeCPUs(n)
	for i, a := range AllowedCPUs {
		if a == n {
			return i
		}
	}
	return 0
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Runner executes a binary and returns combined stdout. Swappable in tests.
type Runner func(name string, args ...string) (string, error)

// DefaultRunner shells out to the real binary.
func DefaultRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// SqueueFormat is the column order for our parseable squeue call.
// NOTE: squeue wants the lowercase --format (-o) flag for %-codes.
// The capitalized --Format (-O) flag expects new-style field names
// (JobID,Name,...) and rejects %-codes with
// "Invalid job format specification".
const SqueueFormat = "%i|%j|%u|%t|%T|%M|%l|%D|%C|%m|%P|%R|%q|%S|%V"

// SqueueArgs builds the squeue invocation.
func SqueueArgs(userOnly bool) []string {
	args := []string{"--noheader", "--format=" + SqueueFormat}
	if userOnly {
		args = append([]string{"--me"}, args...)
	}
	return args
}

// FetchJobs runs squeue and parses the result.
func FetchJobs(run Runner, userOnly bool) ([]Job, string, error) {
	if run == nil {
		run = DefaultRunner
	}
	out, err := run("squeue", SqueueArgs(userOnly)...)
	if err != nil {
		// squeue exits non-zero when there are no jobs on some clusters
		// with "slurm_load_jobs error: Invalid job id specified"-style
		// messages; treat empty output as zero jobs.
		if strings.TrimSpace(out) == "" {
			return nil, out, nil
		}
		return nil, out, fmt.Errorf("squeue: %w: %s", err, strings.TrimSpace(out))
	}
	jobs, perr := ParseSqueue(out)
	if perr != nil {
		return jobs, out, perr
	}
	if len(jobs) == 0 && isSqueueError(out) {
		return nil, out, fmt.Errorf("squeue: %s", firstLine(out))
	}
	return jobs, out, nil
}

// isSqueueError reports whether squeue printed an error instead of rows.
// NOTE: squeue can exit 0 even on errors (e.g. a bad --format string),
// so a non-empty-but-unparseable output must surface as an error rather
// than a silently empty job list.
func isSqueueError(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "squeue:") ||
		strings.Contains(lower, "slurm_load_jobs error") ||
		strings.Contains(lower, "error:")
}

func firstLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return strings.TrimSpace(out)
}

// validJobID matches real squeue job ids: 12345, 12345_1 (arrays),
// 12345.0 (steps). Anything else (squeue error text, stray headers)
// is skipped so it can never become a Job or a log path component.
var validJobID = regexp.MustCompile(`^[0-9]+([_.][0-9\[\]-]+)?(\.[0-9]+)?$`)

// ParseSqueue parses `squeue --noheader --format=...` output.
// Lines whose first field is not a plausible job id are skipped.
func ParseSqueue(out string) ([]Job, error) {
	var jobs []Job
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		if len(parts) == 0 || !validJobID.MatchString(parts[0]) {
			continue
		}
		if len(parts) < 13 {
			// Be lenient: pad short rows (older slurm may lack fields).
			for len(parts) < 15 {
				parts = append(parts, "")
			}
		}
		j := Job{
			ID:         parts[0],
			Name:       parts[1],
			User:       parts[2],
			StateShort: parts[3],
			State:      parts[4],
			TimeUsed:   parts[5],
			TimeLimit:  parts[6],
			Nodes:      parts[7],
			CPUs:       parts[8],
			Mem:        parts[9],
			Partition:  parts[10],
			NodeList:   parts[11],
			QOS:        parts[12],
		}
		if len(parts) > 14 {
			// 15-column layout: %S start, %V submit.
			j.StartTime = parts[13]
			j.SubmitTime = parts[14]
		} else if len(parts) > 13 {
			// Legacy 14-column layout had submit time in %S slot.
			j.SubmitTime = parts[13]
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

var sbatchJobID = regexp.MustCompile(`Submitted batch job (\S+)`)

// ParseSbatchOutput extracts the job id from sbatch stdout.
func ParseSbatchOutput(out string) (string, error) {
	m := sbatchJobID.FindStringSubmatch(out)
	if len(m) < 2 {
		return "", fmt.Errorf("could not parse job id from sbatch output: %q", strings.TrimSpace(out))
	}
	return strings.TrimSuffix(m[1], "."), nil
}

// SafeJobName strips characters slurm/log paths dislike.
func SafeJobName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "lsjob"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ' || r == '\t':
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "lsjob"
	}
	return b.String()
}

// SafeJobID strips anything that must never reach the filesystem:
// path separators, spaces, shell/glob metacharacters. Real slurm ids
// (digits, `_`, `.`, array brackets) pass through untouched.
func SafeJobID(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r == '-', r == '_', r == '.', r == '[', r == ']':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// OutPath / ErrPath are the concrete log files for a known job id.
func OutPath(logDir, jobName, jobID string) string {
	return filepath.Join(logDir, fmt.Sprintf("%s-%s.out", SafeJobName(jobName), SafeJobID(jobID)))
}

// ErrPath mirrors OutPath for stderr.
func ErrPath(logDir, jobName, jobID string) string {
	return filepath.Join(logDir, fmt.Sprintf("%s-%s.err", SafeJobName(jobName), SafeJobID(jobID)))
}

// ResolveLogPath finds the best log file for a job: exact <name>-<id>.out,
// then .err, then any file in logDir containing the id.
func ResolveLogPath(logDir, jobName, jobID string, wantErr bool) string {
	jobID = SafeJobID(jobID)
	if jobID == "" {
		return ""
	}
	candidates := []string{OutPath(logDir, jobName, jobID), ErrPath(logDir, jobName, jobID)}
	if wantErr {
		candidates[0], candidates[1] = candidates[1], candidates[0]
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	// Fallback: any log file mentioning this job id (covers jobs submitted
	// outside this tool or with a custom --output template).
	matches, _ := filepath.Glob(filepath.Join(logDir, "*"+jobID+"*"))
	if len(matches) > 0 {
		return matches[0]
	}
	// Nothing on disk yet: report where we expect it.
	if wantErr {
		return ErrPath(logDir, jobName, jobID)
	}
	return OutPath(logDir, jobName, jobID)
}

// BuildSbatchScript renders the sbatch file. The app injects --output/--error
// itself so `squeue`-listed jobs always have a viewable log.
func BuildSbatchScript(o SubmitOptions) string {
	name := SafeJobName(o.JobName)
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	fmt.Fprintf(&b, "#SBATCH --job-name=%s\n", name)
	if o.WorkDir != "" {
		fmt.Fprintf(&b, "#SBATCH --chdir=%s\n", o.WorkDir)
	}
	fmt.Fprintf(&b, "#SBATCH --output=%s\n", filepath.Join(o.LogDir, name+"-%j.out"))
	fmt.Fprintf(&b, "#SBATCH --error=%s\n", filepath.Join(o.LogDir, name+"-%j.err"))
	if o.Partition != "" {
		fmt.Fprintf(&b, "#SBATCH --partition=%s\n", o.Partition)
	}
	if o.CPUs > 0 {
		fmt.Fprintf(&b, "#SBATCH --cpus-per-task=%d\n", NormalizeCPUs(o.CPUs))
	}
	if o.Time != "" {
		fmt.Fprintf(&b, "#SBATCH --time=%s\n", o.Time)
	}
	b.WriteString("\n")
	b.WriteString(o.Command)
	b.WriteString("\n")
	return b.String()
}

// SubmitJob submits o.Command and returns the job id. A lone existing
// file containing #SBATCH directives (e.g. benzene_au.sh, optionally
// prefixed with "sbatch") is submitted directly via `sbatch file` —
// with only --job-name/--output/--error overridden so the app's log
// viewer keeps working and the script's own resource directives
// (cpus, ntasks, mail, …) are respected. Anything else is wrapped in a
// generated sbatch script pinned to WorkDir via --chdir.
func SubmitJob(run Runner, o SubmitOptions) (string, string, error) {
	if run == nil {
		run = DefaultRunner
	}
	if strings.TrimSpace(o.Command) == "" {
		return "", "", fmt.Errorf("empty command")
	}
	if o.WorkDir == "" {
		if wd, err := os.Getwd(); err == nil {
			o.WorkDir = wd
		}
	}
	if o.LogDir != "" {
		if err := os.MkdirAll(o.LogDir, 0o755); err != nil {
			return "", "", err
		}
	}
	if scriptPath, jobName, ok := DetectScript(o.WorkDir, o.Command); ok {
		return submitScript(run, o, scriptPath, jobName)
	}
	if fields := strings.Fields(o.Command); len(fields) > 0 && fields[0] == "sbatch" {
		return "", "", fmt.Errorf("sbatch options are not supported: enter just the script path (e.g. benzene_au.sh)")
	}
	o.Command = EffectiveCommand(o.WorkDir, o.Command)
	script := BuildSbatchScript(o)
	tmp, err := os.CreateTemp("", "lazyslurm-*.sbatch")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(script); err != nil {
		tmp.Close()
		return "", "", err
	}
	tmp.Close()
	out, err := run("sbatch", tmpName)
	if err != nil {
		return "", out, fmt.Errorf("sbatch: %w: %s", err, strings.TrimSpace(out))
	}
	id, err := ParseSbatchOutput(out)
	if err != nil {
		return "", out, err
	}
	return id, out, nil
}

var (
	sbatchLine = regexp.MustCompile(`(?m)^\s*#SBATCH\b`)
	jobNameEq  = regexp.MustCompile(`(?m)^\s*#SBATCH\s+--job-name=([^\s]+)`)
	jobNameSp  = regexp.MustCompile(`(?m)^\s*#SBATCH\s+--job-name\s+([^\s]+)`)
	jobNameSh  = regexp.MustCompile(`(?m)^\s*#SBATCH\s+-J\s*=?\s*([^\s]+)`)
)

// DetectScript reports whether cmd means "submit this sbatch script file":
// a single existing regular file — optionally after a leading "sbatch" —
// containing #SBATCH directives. Returns the absolute script path and the
// job name from its --job-name (or the file basename as fallback).
func DetectScript(workDir, cmd string) (scriptPath, jobName string, ok bool) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", "", false
	}
	target := ""
	if fields[0] == "sbatch" {
		if len(fields) != 2 {
			return "", "", false
		}
		target = fields[1]
	} else {
		if len(fields) != 1 {
			return "", "", false
		}
		target = fields[0]
	}
	path := resolveWorkPath(workDir, target)
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() > 1<<20 {
		return "", "", false
	}
	data, err := os.ReadFile(path)
	if err != nil || !sbatchLine.Match(data) {
		return "", "", false
	}
	name := ""
	for _, re := range []*regexp.Regexp{jobNameEq, jobNameSp, jobNameSh} {
		if m := re.FindSubmatch(data); m != nil {
			name = string(m[1])
			break
		}
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return path, SafeJobName(name), true
}

// submitScript submits an existing sbatch script file, overriding only the
// options the app needs: --job-name (deterministic %x for logs), and
// --output/--error into the app log dir. All other script directives
// (cpus, ntasks, time, mail, …) are respected as written.
func submitScript(run Runner, o SubmitOptions, scriptPath, jobName string) (string, string, error) {
	name := SafeJobName(jobName)
	if name == "" {
		name = "lsjob"
	}
	args := []string{
		"--job-name=" + name,
		"--output=" + filepath.Join(o.LogDir, name+"-%j.out"),
		"--error=" + filepath.Join(o.LogDir, name+"-%j.err"),
		scriptPath,
	}
	out, err := run("sbatch", args...)
	if err != nil {
		return "", out, fmt.Errorf("sbatch: %w: %s", err, strings.TrimSpace(out))
	}
	id, err := ParseSbatchOutput(out)
	if err != nil {
		return "", out, err
	}
	return id, out, nil
}

// EffectiveCommand rewrites cmd the way it will actually run in wrap mode:
// a bare name that is an executable file in workDir but not on PATH gets a
// ./ prefix (bare names never resolve via PATH). Script submissions and
// everything else pass through untouched.
func EffectiveCommand(workDir, cmd string) string {
	if _, _, ok := DetectScript(workDir, cmd); ok {
		return cmd
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return cmd
	}
	first := fields[0]
	if strings.Contains(first, "/") {
		return cmd
	}
	if !isExecFile(resolveWorkPath(workDir, first)) {
		return cmd
	}
	if _, err := exec.LookPath(first); err == nil {
		return cmd // found on PATH: keep shell semantics
	}
	fields[0] = "./" + first
	return strings.Join(fields, " ")
}

func isExecFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir() && st.Mode().Perm()&0o111 != 0
}

func resolveWorkPath(workDir, p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~/"))
		}
	} else if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workDir, p)
}

// CancelJob runs scancel <id>.
func CancelJob(run Runner, jobID string) (string, error) {
	if run == nil {
		run = DefaultRunner
	}
	out, err := run("scancel", jobID)
	if err != nil {
		return out, fmt.Errorf("scancel: %w: %s", err, strings.TrimSpace(out))
	}
	return out, nil
}

// ParseScontrol parses `scontrol show job` Key=Value output into a map.
func ParseScontrol(out string) map[string]string {
	m := map[string]string{}
	// scontrol separates pairs with spaces, values may be quoted.
	// A simple fields split works for display purposes.
	for _, f := range strings.Fields(out) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}

// FetchScontrol runs `scontrol show job <id>` best-effort (may fail for old jobs).
func FetchScontrol(run Runner, jobID string) (map[string]string, error) {
	if run == nil {
		run = DefaultRunner
	}
	out, err := run("scontrol", "show", "job", jobID)
	if err != nil {
		return nil, fmt.Errorf("scontrol: %w", err)
	}
	return ParseScontrol(out), nil
}
