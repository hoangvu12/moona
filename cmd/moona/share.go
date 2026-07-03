package main

import (
	"fmt"
	"os"
)

// runShare handles `moona` (no args) and `moona share [flags]`. It creates a
// session running the default shell (or --cmd / -- <cmd>) in the daemon and
// attaches the current terminal to it.
func runShare(args []string) error {
	cfg, err := parseShareConfig(args)
	if err != nil {
		return err
	}
	return startSessionAndAttach(cfg)
}

// runShortcut handles `moona <command> [args...]` (e.g. `moona claude`). Unlike
// the old flow there is no QR to protect and no Enter prompt: the QR lives in
// the daemon, so the command starts immediately.
func runShortcut(commandArgs []string) error {
	cfg, err := parseShortcutConfig(commandArgs)
	if err != nil {
		return err
	}
	return startSessionAndAttach(cfg)
}

// startSessionAndAttach ensures a daemon is running, asks it to spawn a session
// for cfg.commandLine, then attaches the current terminal to that session. The
// session lives in the daemon and survives this terminal closing.
func startSessionAndAttach(cfg config) error {
	opts := daemonOptions{
		host:   cfg.host,
		port:   cfg.port,
		token:  cfg.token,
		tunnel: cfg.tunnel,
		qr:     cfg.qr,
	}
	st, err := ensureDaemon(opts)
	if err != nil {
		return err
	}

	// Seed the session with this console's size; once attached, this terminal is
	// the primary (size authority) anyway.
	cols, rows := currentConsoleSize()
	if cols <= 0 || rows <= 0 {
		cols, rows = cfg.cols, cfg.rows
	}

	id, err := newAPIClient(st).createSession(cfg.commandLine, cfg.workDir, cols, rows)
	if err != nil {
		return err
	}

	if st.PublicURL != "" {
		fmt.Fprintf(os.Stderr, "moona: session %s attached. Phone: %s  (moona qr for code)\r\n", id, browserURL(st.PublicURL, st.Token))
	} else {
		fmt.Fprintf(os.Stderr, "moona: session %s attached. Local: %s  (moona qr / moona url)\r\n", id, browserURL(st.LocalURL, st.Token))
	}

	return attachSession(st, id, true)
}
