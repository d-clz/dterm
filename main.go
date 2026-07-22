package main

import (
	"flag"
	"fmt"
	"os"

	"dterm/internal/config"
	"dterm/internal/dlog"
	"dterm/internal/ui"
)

func main() {
	dlog.Init()
	cfgPath := flag.String("config", "", "path to bookmarks.yaml (default: ~/.dterm/bookmarks.yaml)")
	flag.Parse()

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
