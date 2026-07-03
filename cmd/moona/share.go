package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func runShare(args []string) error {
	cfg, err := parseShareConfig(args)
	if err != nil {
		return err
	}

	running, err := startShareServer(cfg)
	if err != nil {
		return err
	}
	defer running.shutdown()

	if err := waitForShare(running.localURL, 2*time.Second); err != nil {
		return err
	}
	if err := maybeStartTunnel(running); err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}

	printShareBanner(running)
	return <-running.errc
}

func runShortcut(commandArgs []string) error {
	cfg, err := parseShortcutConfig(commandArgs)
	if err != nil {
		return err
	}

	running, err := startShareServer(cfg)
	if err != nil {
		return err
	}
	defer running.shutdown()

	if err := waitForShare(running.localURL, 2*time.Second); err != nil {
		return err
	}
	if err := maybeStartTunnel(running); err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}

	terminalPrintln(os.Stdout, "Moona shortcut started")
	terminalPrintln(os.Stdout, "Local:  ", running.localURL)
	if running.publicURL != "" {
		terminalPrintln(os.Stdout, "Public: ", running.publicURL)
	} else {
		terminalPrintln(os.Stdout, "Phone:   tunnel unavailable/disabled; local URL only")
	}
	terminalPrintln(os.Stdout, "Command:", cfg.commandLine)
	terminalPrintln(os.Stdout)
	printPhoneQR(running)
	proceed, err := waitForShortcutConfirmation(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	if !proceed {
		terminalPrintln(os.Stdout, "Cancelled. Moona stopped before starting the command.")
		return nil
	}
	clearTerminalScreen(os.Stdout)
	running.sess.allowStart()
	if err := running.sess.ensureStarted(); err != nil {
		return fmt.Errorf("start terminal: %w", err)
	}

	attachArgs := []string{"--url", running.localURL, "--quiet"}
	if cfg.token != "" {
		attachArgs = append(attachArgs, "--token", cfg.token)
	}
	return runAttach(attachArgs)
}

func waitForShortcutConfirmation(reader io.Reader, writer io.Writer) (bool, error) {
	restoreConfirmationInput, _ := prepareShortcutConfirmationInput()
	defer restoreConfirmationInput()

	terminalPrintln(writer)
	terminalPrintf(writer, "Press Enter to start the command and attach, or type q then Enter to quit: ")
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer != "q" && answer != "quit" && answer != "exit", nil
}

func printShareBanner(running *runningShare) {
	cfg := running.cfg
	terminalPrintln(os.Stdout, "Moona terminal is running")
	terminalPrintln(os.Stdout, "Local:  ", running.localURL)
	if running.publicURL != "" {
		terminalPrintln(os.Stdout, "Public: ", running.publicURL)
	}
	terminalPrintln(os.Stdout, "Command:", cfg.commandLine)
	terminalPrintln(os.Stdout, "CWD:    ", cfg.workDir)
	if cfg.token != "" {
		terminalPrintln(os.Stdout, "Token:  ", "enabled; QR/open URL includes the token")
	}
	terminalPrintln(os.Stdout)
	if running.publicURL == "" {
		terminalPrintln(os.Stdout, "Temporary tunnel is unavailable; check warnings above or use --tunnel=false for local-only.")
	} else {
		terminalPrintln(os.Stdout, "Temporary tunnel is running. Keep this process open while using the phone URL.")
	}
	terminalPrintln(os.Stdout)
	printPhoneQR(running)
	terminalPrintln(os.Stdout, "Attach from a normal Windows Terminal with:")
	terminalPrintf(os.Stdout, "  moona attach --url %s\r\n", running.localURL)
	terminalPrintln(os.Stdout)
	terminalPrintln(os.Stdout, "Press Ctrl+C in this window to stop the shared terminal.")
}
