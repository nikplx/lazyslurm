package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeStat(existing map[string]bool) func(string) (os.FileInfo, error) {
	return func(p string) (os.FileInfo, error) {
		if existing[p] {
			return fakeDirInfo{}, nil
		}
		return nil, os.ErrNotExist
	}
}

type fakeDirInfo struct{ os.FileInfo }

func (fakeDirInfo) IsDir() bool { return true }

func TestBaseDirPrefersBeegfs(t *testing.T) {
	stat := fakeStat(map[string]bool{"/beegfs/u/alice": true})
	if got := baseDir("/home/alice", "alice", stat); got != "/beegfs/u/alice/.lazyslurm" {
		t.Fatalf("got %q", got)
	}
}

func TestBaseDirFallsBackHome(t *testing.T) {
	stat := fakeStat(nil)
	if got := baseDir("/home/alice", "alice", stat); got != "/home/alice/.lazyslurm" {
		t.Fatalf("got %q", got)
	}
	if got := baseDir("/home/alice", "", stat); got != "/home/alice/.lazyslurm" {
		t.Fatalf("empty user: got %q", got)
	}
}

func TestDefaultPathMigratesFromLegacy(t *testing.T) {
	stat := fakeStat(map[string]bool{"/home/alice/.remoteslurm/config.json": true})
	if got := defaultPath("/home/alice", stat); got != "/home/alice/.remoteslurm/config.json" {
		t.Fatalf("got %q", got)
	}
	stat = fakeStat(map[string]bool{
		"/home/alice/.lazyslurm/config.json":   true,
		"/home/alice/.remoteslurm/config.json": true,
	})
	if got := defaultPath("/home/alice", stat); got != "/home/alice/.lazyslurm/config.json" {
		t.Fatalf("new path should win, got %q", got)
	}
}

func TestLoadToleratesRemovedKeys(t *testing.T) {
	// Old config files may still carry default_mem / a home-based history
	// path; both must load without error. Explicit paths are respected.
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	content := `{"default_mem": "4G", "default_cpus": 16, "log_dir": "` + dir + `/logs"}`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Resolve()
	if cfg.DefaultCPUs != 16 {
		t.Fatalf("cpus = %d", cfg.DefaultCPUs)
	}
	if !strings.HasSuffix(cfg.LogDir, "logs") {
		t.Fatalf("logdir = %q", cfg.LogDir)
	}
}
