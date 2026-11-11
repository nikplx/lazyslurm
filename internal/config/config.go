package config

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Config holds user-tunable defaults.
type Config struct {
	DefaultPartition string `json:"default_partition"`
	DefaultCPUs      int    `json:"default_cpus"`
	DefaultTime      string `json:"default_time"`
	DefaultJobName   string `json:"default_job_name"`
	LogDir           string `json:"log_dir"`
	HistoryFile      string `json:"history_file"`
	PollSecs         int    `json:"poll_interval_secs"`
	UserOnly         bool   `json:"squeue_user_only"`
}

// Default returns sane defaults rooted at the preferred base dir
// (see DefaultBase).
func Default() Config {
	base := DefaultBase()
	return Config{
		DefaultPartition: "",
		DefaultCPUs:      8,
		DefaultTime:      "01:00:00",
		DefaultJobName:   "lsjob",
		LogDir:           filepath.Join(base, "logs"),
		HistoryFile:      filepath.Join(base, "history.json"),
		PollSecs:         2,
		UserOnly:         true,
	}
}

// DefaultBase is ~/.lazyslurm — unless per-user beegfs scratch exists
// (e.g. /beegfs/u/<user> on Hummel), in which case it is
// /beegfs/u/<user>/.lazyslurm. Slurm compute nodes often cannot write
// to home NFS, so job logs must live on the writable scratch filesystem.
func DefaultBase() string {
	home, _ := os.UserHomeDir()
	return baseDir(home, currentUser(), os.Stat)
}

func baseDir(home, user string, stat func(string) (os.FileInfo, error)) string {
	if user != "" {
		if fi, err := stat(filepath.Join("/beegfs/u", user)); err == nil && fi.IsDir() {
			return filepath.Join("/beegfs/u", user, ".lazyslurm")
		}
	}
	return filepath.Join(home, ".lazyslurm")
}

func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return os.Getenv("LOGNAME")
}

// DefaultPath is ~/.lazyslurm/config.json, falling back to the legacy
// ~/.remoteslurm/config.json when the new path doesn't exist yet.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return defaultPath(home, os.Stat)
}

func defaultPath(home string, stat func(string) (os.FileInfo, error)) string {
	path := filepath.Join(home, ".lazyslurm", "config.json")
	if _, err := stat(path); err == nil {
		return path
	}
	legacy := filepath.Join(home, ".remoteslurm", "config.json")
	if _, err := stat(legacy); err == nil {
		return legacy
	}
	return path
}

// Load reads path if it exists, otherwise returns defaults.
// Missing keys keep default values.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(expandPath(path))
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	// Merge over defaults so omitted keys survive.
	merged := Default()
	dec := map[string]any{}
	if err := json.Unmarshal(data, &dec); err != nil {
		return cfg, err
	}
	raw, _ := json.Marshal(dec)
	if err := json.Unmarshal(raw, &merged); err != nil {
		return cfg, err
	}
	// Preserve defaults for zero values that were omitted is handled by
	// starting from Default and only unmarshalling present keys above…
	// json.Unmarshal into merged overwrites with zero values for missing
	// keys, so re-apply defaults for empties.
	def := Default()
	if _, ok := dec["default_partition"]; !ok {
		merged.DefaultPartition = def.DefaultPartition
	}
	if _, ok := dec["default_cpus"]; !ok {
		merged.DefaultCPUs = def.DefaultCPUs
	}
	if _, ok := dec["default_time"]; !ok {
		merged.DefaultTime = def.DefaultTime
	}
	if _, ok := dec["default_job_name"]; !ok {
		merged.DefaultJobName = def.DefaultJobName
	}
	if _, ok := dec["log_dir"]; !ok {
		merged.LogDir = def.LogDir
	}
	if _, ok := dec["history_file"]; !ok {
		merged.HistoryFile = def.HistoryFile
	}
	if _, ok := dec["poll_interval_secs"]; !ok {
		merged.PollSecs = def.PollSecs
	}
	if _, ok := dec["squeue_user_only"]; !ok {
		merged.UserOnly = def.UserOnly
	}
	return merged, nil
}

// Resolve expands ~ and makes LogDir / HistoryFile absolute.
func (c *Config) Resolve() {
	c.LogDir = expandPath(c.LogDir)
	c.HistoryFile = expandPath(c.HistoryFile)
	if c.DefaultCPUs <= 0 {
		c.DefaultCPUs = 8
	}
	if c.PollSecs <= 0 {
		c.PollSecs = 2
	}
	if c.DefaultTime == "" {
		c.DefaultTime = "01:00:00"
	}
	if c.DefaultJobName == "" {
		c.DefaultJobName = "lsjob"
	}
}

// EnsureDirs creates log dir and history parent.
func (c *Config) EnsureDirs() error {
	if err := os.MkdirAll(c.LogDir, 0o755); err != nil {
		return err
	}
	if dir := filepath.Dir(c.HistoryFile); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~/"))
	}
	if p == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	return p
}
