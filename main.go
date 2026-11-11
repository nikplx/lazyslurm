package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"lazyslurm/internal/config"
	"lazyslurm/internal/history"
	"lazyslurm/internal/tui"
)

var version = "dev"

// migrateLegacyHistory copies ~/.remoteslurm/history.json (the old project
// name) to the configured
// location when the latter doesn't exist yet (e.g. after the default base
// moved to beegfs scratch). Best effort: failures are ignored since the
// store treats a missing file as empty history.
func migrateLegacyHistory(dst string) {
	home, _ := os.UserHomeDir()
	legacy := filepath.Join(home, ".remoteslurm", "history.json")
	if legacy == dst {
		return
	}
	if _, err := os.Stat(dst); err == nil {
		return
	}
	data, err := os.ReadFile(legacy)
	if err != nil || len(data) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(dst, data, 0o644)
}

func main() {
	configPath := flag.String("config", config.DefaultPath(), "path to config.json")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("lazyslurm", version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lazyslurm: bad config %s: %v\n", *configPath, err)
		os.Exit(1)
	}
	cfg.Resolve()
	migrateLegacyHistory(cfg.HistoryFile)
	if err := cfg.EnsureDirs(); err != nil {
		fmt.Fprintf(os.Stderr, "lazyslurm: cannot create dirs: %v\n", err)
		os.Exit(1)
	}

	store := history.New(cfg.HistoryFile)
	model := tui.New(cfg, store, nil)
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "lazyslurm: %v\n", err)
		os.Exit(1)
	}
}
