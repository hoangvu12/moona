package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
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
	// First-ever use: run the setup wizard before starting the daemon so the phone
	// link is what the user chose. If they quit it (or it can't run, e.g. no TTY),
	// fall through and start the session with defaults — typing `moona claude`
	// should never be blocked by setup.
	_, _ = maybeRunFirstRunSetup()

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
	interactive := cols > 0 && rows > 0

	// Show the connect-phone panel (QR + link + code) with an explicit choice:
	// open the session, or quit. We do this BEFORE creating the session so that
	// quitting leaves nothing behind. Skipped when there is no interactive console
	// (piped/scripted use), where we just print a one-line hint after starting.
	if interactive && !showConnectPanel(st) {
		return nil // user chose to quit before starting a session
	}

	id, err := newAPIClient(st).createSession(cfg.commandLine, cfg.workDir, cols, rows)
	if err != nil {
		return err
	}

	if !interactive {
		fmt.Fprintf(os.Stderr, "moona: session %s — phone: %s  (run `moona qr` to show the code)\r\n", id, browserURL(bestPhoneTarget(st), st.Token))
	}

	return attachSession(st, id, true)
}

// bestPhoneTarget returns the URL a phone should open: the public tunnel if there
// is one, otherwise the local URL (same-network only).
func bestPhoneTarget(st daemonState) string {
	if st.PublicURL != "" {
		return st.PublicURL
	}
	return st.LocalURL
}

// showConnectPanel prints a QR code + link (+ access code) for connecting a phone,
// then asks the user to either open the session or quit. It is the first thing a
// new user sees, so it must make "how do I open this on my phone" obvious. Returns
// true to proceed (open the session), false to quit without starting anything.
func showConnectPanel(st daemonState) bool {
	target := bestPhoneTarget(st)
	label := "Scan on your phone"
	if st.PublicURL == "" {
		label = "Scan on your phone (same Wi-Fi only)"
	}
	link := browserURL(target, st.Token)

	clearTerminalScreen(os.Stdout)
	terminalPrintln(os.Stdout, "  Connect your phone")
	terminalPrintln(os.Stdout)
	terminalPrintf(os.Stdout, "%s", renderTerminalQR(link))
	terminalPrintln(os.Stdout)
	terminalPrintln(os.Stdout, "  "+label+":")
	terminalPrintln(os.Stdout, "    "+link)
	if st.Token != "" {
		terminalPrintln(os.Stdout)
		terminalPrintln(os.Stdout, "  Scanning fills in the access code for you. If you type the address")
		terminalPrintln(os.Stdout, "  by hand instead, enter this code once:  "+st.Token)
	}
	if !isOnboarded() {
		terminalPrintln(os.Stdout)
		terminalPrintln(os.Stdout, "  Tip: run `moona setup` for a permanent link that survives restarts.")
	}
	terminalPrintln(os.Stdout)
	terminalPrintln(os.Stdout, "  Show this panel again anytime with:  moona qr")
	terminalPrintln(os.Stdout)
	fmt.Fprint(os.Stdout, "  Press Enter to open your session, or type q to quit: ")

	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "q", "quit":
		return false
	default:
		return true
	}
}
