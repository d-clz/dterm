package main

import (
	"flag"
	"fmt"
	"os"

	"dterm/internal/config"
	"dterm/internal/dlog"
	"dterm/internal/ui"
)

// Set via -ldflags at release build time (see .goreleaser.yaml).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	dlog.Init()
	cfgPath := flag.String("config", "", "path to bookmarks.yaml (default: ~/.dterm/bookmarks.yaml)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("dterm %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	if err := ui.Run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
