package slurm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSqueueArgsUseLowercaseFormat(t *testing.T) {
	// Regression test: squeue's capitalized --Format expects new-style
	// field names and rejects %-codes with
	// "Invalid job format specification: %i". The lowercase --format
	// is the one that accepts %-codes.
	args := SqueueArgs(true)
	found := false
	for _, a := range args {
		if strings.HasPrefix(a, "--Format=") {
			t.Fatalf("must not use capitalized --Format: %v", args)
		}
		if a == "--format="+SqueueFormat {
			found = true
		}
	}
	if !found {
		t.Fatalf("args missing --format: %v", args)
	}
}

func TestParseSqueue(t *testing.T) {
	out := "12345|myjob|alice|R|RUNNING|00:12:34|01:00:00|1|4|8G|gpu|node01|normal|2026-09-26T10:01:00|2026-09-26T10:00:00\n" +
		"12346|other|bob|PD|PENDING|00:00:00|02:00:00|1|1|4G|cpu|n/a|normal|N/A|2026-09-26T10:05:00\n"
	jobs, err := ParseSqueue(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	if jobs[0].ID != "12345" || jobs[0].State != "RUNNING" || jobs[0].CPUs != "4" || jobs[0].Mem != "8G" {
		t.Fatalf("bad first job: %+v", jobs[0])
	}
	if jobs[0].SubmitTime != "2026-09-26T10:00:00" || jobs[0].StartTime != "2026-09-26T10:01:00" {
		t.Fatalf("bad times: %+v", jobs[0])
	}
	if jobs[1].StateShort != "PD" || jobs[1].Partition != "cpu" {
		t.Fatalf("bad second job: %+v", jobs[1])
	}
	if _, err := ParseSqueue(""); err != nil {
		t.Fatal(err)
	}
}

func TestParseSqueueSkipsGarbage(t *testing.T) {
	// Regression test: an squeue failure line must never become a Job
	// (it previously ended up inside a log path).
	out := "squeue: error: Invalid job format specification: %i\n" +
		"JOBID|NAME|USER|ST|STATE|TIME|TIME_LIMIT|NODES|CPUS|MIN_MEMORY|PARTITION|NODELIST|QOS|START|SUBMIT\n" +
		"777|ok|alice|R|RUNNING|00:00:01|01:00:00|1|1|1G|cpu|node01|normal|2026-09-26T10:01:00|2026-09-26T10:00:00\n"
	jobs, err := ParseSqueue(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "777" {
		t.Fatalf("got %+v", jobs)
	}
}

func TestFetchJobsSurfacesSqueueError(t *testing.T) {
	// squeue exits 0 even on format errors, so FetchJobs must turn an
	// unparseable error output into an error, not an empty list.
	errRun := func(name string, args ...string) (string, error) {
		return "squeue: error: Invalid job format specification: %i\n", nil
	}
	if _, _, err := FetchJobs(errRun, true); err == nil {
		t.Fatal("expected error for squeue error output")
	}
	emptyRun := func(name string, args ...string) (string, error) {
		return "", nil
	}
	jobs, _, err := FetchJobs(emptyRun, true)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("empty squeue output must mean zero jobs, got %v %v", jobs, err)
	}
}

func TestParseSbatchOutput(t *testing.T) {
	id, err := ParseSbatchOutput("Submitted batch job 98765\n")
	if err != nil || id != "98765" {
		t.Fatalf("got %q %v", id, err)
	}
	if _, err := ParseSbatchOutput("sbatch: error: wat"); err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildSbatchScriptInjectsLogs(t *testing.T) {
	s := BuildSbatchScript(SubmitOptions{
		Command: "python train.py", JobName: "exp1", Partition: "gpu",
		CPUs: 16, Time: "02:00:00", LogDir: "/logs",
	})
	for _, want := range []string{
		"#SBATCH --job-name=exp1",
		"#SBATCH --output=/logs/exp1-%j.out",
		"#SBATCH --error=/logs/exp1-%j.err",
		"#SBATCH --partition=gpu",
		"#SBATCH --cpus-per-task=16",
		"#SBATCH --time=02:00:00",
		"python train.py",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("script missing %q:\n%s", want, s)
		}
	}
	// No mem setting: memory is sized by the cluster from the CPU count.
	if strings.Contains(s, "--mem") {
		t.Fatalf("script must not set memory:\n%s", s)
	}
	// Odd CPU counts are snapped to the allowed 8/16/32.
	s = BuildSbatchScript(SubmitOptions{Command: "x", JobName: "j", CPUs: 4, LogDir: "/logs"})
	if !strings.Contains(s, "#SBATCH --cpus-per-task=8\n") {
		t.Fatalf("CPUs not snapped:\n%s", s)
	}
}

func TestNormalizeCPUs(t *testing.T) {
	if len(AllowedCPUs) != 24 || AllowedCPUs[0] != 8 || AllowedCPUs[23] != 192 {
		t.Fatalf("AllowedCPUs = %v", AllowedCPUs)
	}
	for in, want := range map[int]int{0: 8, -3: 8, 1: 8, 8: 8, 11: 8, 12: 8, 13: 16, 16: 16, 20: 16, 21: 24, 24: 24, 100: 96, 189: 192, 200: 192} {
		if got := NormalizeCPUs(in); got != want {
			t.Fatalf("NormalizeCPUs(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestSubmitJobEmptyCommand(t *testing.T) {
	if _, _, err := SubmitJob(nil, SubmitOptions{}); err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestSubmitJobChdirAndWorkDir(t *testing.T) {
	// The job must run in the TUI's launch directory, not wherever the
	// scheduler would otherwise place it.
	var script string
	run := func(name string, args ...string) (string, error) {
		if name != "sbatch" || len(args) == 0 {
			t.Fatalf("unexpected call %s %v", name, args)
		}
		data, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			t.Fatal(err)
		}
		script = string(data)
		return "Submitted batch job 42\n", nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := SubmitJob(run, SubmitOptions{Command: "./run.sh", JobName: "j", LogDir: t.TempDir()})
	if err != nil || id != "42" {
		t.Fatalf("got %q %v", id, err)
	}
	if !strings.Contains(script, "#SBATCH --chdir="+cwd+"\n") {
		t.Fatalf("script missing --chdir:\n%s", script)
	}
	// Explicit WorkDir wins over cwd.
	id, _, err = SubmitJob(run, SubmitOptions{Command: "x", JobName: "j", LogDir: t.TempDir(), WorkDir: "/proj/x"})
	if err != nil || id != "42" {
		t.Fatalf("got %q %v", id, err)
	}
	if !strings.Contains(script, "#SBATCH --chdir=/proj/x\n") {
		t.Fatalf("script missing explicit --chdir:\n%s", script)
	}
}

func TestResolveLogPath(t *testing.T) {
	dir := t.TempDir()
	// exact .out wins
	if err := os.WriteFile(filepath.Join(dir, "exp1-111.out"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := ResolveLogPath(dir, "exp1", "111", false); !strings.HasSuffix(p, "exp1-111.out") {
		t.Fatalf("got %q", p)
	}
	// wantErr prefers .err when present
	if err := os.WriteFile(filepath.Join(dir, "exp1-111.err"), []byte("e"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := ResolveLogPath(dir, "exp1", "111", true); !strings.HasSuffix(p, ".err") {
		t.Fatalf("got %q", p)
	}
	// fallback: expected path even when nothing on disk
	if p := ResolveLogPath(dir, "exp1", "999", false); !strings.HasSuffix(p, "exp1-999.out") {
		t.Fatalf("got %q", p)
	}
}

func TestSafeJobName(t *testing.T) {
	if SafeJobName("my experiment 1!") != "my-experiment-1" {
		t.Fatalf("got %q", SafeJobName("my experiment 1!"))
	}
	if SafeJobName("") != "lsjob" {
		t.Fatal("empty should default")
	}
}

func TestSafeJobID(t *testing.T) {
	if SafeJobID("12345") != "12345" {
		t.Fatal("plain id must pass through")
	}
	if SafeJobID("12345_1") != "12345_1" {
		t.Fatal("array id must pass through")
	}
	// Error text must never survive into a file path.
	if got := SafeJobID("squeue: error: Invalid job format specification: %i"); got == "" {
		t.Fatal("expected non-empty sanitized remainder, got empty")
	} else if strings.ContainsAny(got, ": %/") {
		t.Fatalf("unsafe remainder %q", got)
	}
	if SafeJobID("../../etc") != "....etc" {
		t.Fatalf("path separator survived: %q", SafeJobID("../../etc"))
	}
	if SafeJobID("") != "" {
		t.Fatal("empty must stay empty")
	}
}

func writeTestScript(t *testing.T, dir, name, header string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	body := "#!/usr/bin/env bash\n" + header + "\necho hi\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDetectScript(t *testing.T) {
	dir := t.TempDir()
	p := writeTestScript(t, dir, "benzene_au.sh", "#SBATCH --job-name=benzene_au_thc\n#SBATCH --cpus-per-task=96")

	// Bare name, relative path, sbatch-prefixed — all detect.
	for _, cmd := range []string{"benzene_au.sh", "./benzene_au.sh", "sbatch benzene_au.sh", "sbatch ./benzene_au.sh"} {
		got, name, ok := DetectScript(dir, cmd)
		if !ok || got != p || name != "benzene_au_thc" {
			t.Fatalf("DetectScript(%q) = %q %q %v", cmd, got, name, ok)
		}
	}
	// Space-separated --job-name form.
	p2 := writeTestScript(t, dir, "other.sh", "#SBATCH --job-name other_job")
	if _, name, ok := DetectScript(dir, "other.sh"); !ok || name != "other_job" {
		t.Fatalf("space form: %q %v", name, ok)
	}
	_ = p2

	// Rejections: missing file, plain executable, multi-token, sbatch flags.
	if err := os.WriteFile(filepath.Join(dir, "tool"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"missing.sh", "tool", "tool --flag", "sbatch --export=ALL,X=1 benzene_au.sh", "", "echo hi"} {
		if _, _, ok := DetectScript(dir, cmd); ok {
			t.Fatalf("DetectScript(%q) should not match", cmd)
		}
	}
}

func TestSubmitJobScriptMode(t *testing.T) {
	dir := t.TempDir()
	p := writeTestScript(t, dir, "benzene_au.sh", "#SBATCH --job-name=benzene_au_thc\n#SBATCH --cpus-per-task=96")
	var argv []string
	run := func(name string, args ...string) (string, error) {
		argv = append([]string{name}, args...)
		return "Submitted batch job 99\n", nil
	}
	id, _, err := SubmitJob(run, SubmitOptions{Command: "benzene_au.sh", JobName: "ignored", LogDir: t.TempDir(), WorkDir: dir})
	if err != nil || id != "99" {
		t.Fatalf("got %q %v", id, err)
	}
	// Only job-name/output/error overridden; script's own cpus untouched.
	// (%j stays literal: slurm expands it at submit time.)
	wantOut := "benzene_au_thc-%j.out"
	joined := strings.Join(argv, " ")
	for _, want := range []string{"sbatch", "--job-name=benzene_au_thc", wantOut, p} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv missing %q: %v", want, argv)
		}
	}
	if strings.Contains(joined, "--cpus") {
		t.Fatalf("script directives must not be overridden: %v", argv)
	}
	if argv[len(argv)-1] != p {
		t.Fatalf("script must be the last operand: %v", argv)
	}
}

func TestEffectiveCommandDotSlash(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "zz-mytool-1"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Bare executable not on PATH gets ./ prefix…
	if got := EffectiveCommand(dir, "zz-mytool-1 --flag"); got != "./zz-mytool-1 --flag" {
		t.Fatalf("got %q", got)
	}
	// …but PATH binaries keep shell semantics even if shadowed locally.
	if err := os.WriteFile(filepath.Join(dir, "sh"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := EffectiveCommand(dir, "sh -c true"); got != "sh -c true" {
		t.Fatalf("got %q", got)
	}
	// Slashed, missing, and multi-word commands pass through.
	if got := EffectiveCommand(dir, "./zz-mytool-1"); got != "./zz-mytool-1" {
		t.Fatalf("got %q", got)
	}
	if got := EffectiveCommand(dir, "nosuchfile"); got != "nosuchfile" {
		t.Fatalf("got %q", got)
	}
}
