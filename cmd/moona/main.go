package main

import (
	"fmt"
	"log"
	"os"
)

const appName = "moona"
const defaultPort = 8787

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("moona: ")
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		maybeAutoUpdate(args)
		return runShare(args)
	}
	switch args[0] {
	case "daemon", "hub":
		return runDaemon(args[1:])
	case "share", "serve":
		maybeAutoUpdate(args)
		return runShare(args[1:])
	case "attach":
		return runAttach(args[1:])
	case "ls", "list", "sessions":
		return runLs(args[1:])
	case "kill", "close":
		return runKill(args[1:])
	case "url", "link":
		return runURL(args[1:])
	case "qr":
		return runQR(args[1:])
	case "stop":
		return stopDaemon()
	case "status":
		return printDaemonStatus()
	case "update", "upgrade", "self-update":
		return runUpdate(args[1:])
	case "help", "-h", "--help":
		printHelp()
		return nil
	case "version", "--version", "-v":
		printVersion()
		return nil
	default:
		maybeAutoUpdate(args)
		return runShortcut(args)
	}
}

func printHelp() {
	fmt.Println(`Moona - Windows-native web terminal for phone access

Usage:
  moona <command> [args...]      start <command> in a new session and attach (instant)
  moona claude                   e.g. start Claude Code, attached to this terminal
  moona share [flags]            start the default shell in a session and attach
  moona share -- codex           start codex in a session
  moona share --cmd "pwsh.exe"   start a raw command line in a session

  moona daemon [flags]           run the background switchboard (link + QR live here)
  moona daemon stop              stop the switchboard and all sessions
  moona ls                       list active sessions
  moona attach [id]              attach this terminal to a session (default: the only one)
  moona kill <id>                end one session
  moona url | moona qr           reprint the phone link / QR from the running daemon
  moona status                   show whether a daemon is running
  moona update [--check]         update to the latest release (auto on startup)
  moona version

How it works:
  A single background daemon holds every session and serves the phone web page +
  QR. 'moona claude' (and friends) ask the daemon to spawn a session, then attach
  the current terminal to it -- so it opens instantly with no Enter prompt. The
  daemon auto-starts on first use and idle-exits when nothing is connected. Run
  'moona daemon' yourself in a spare tab if you want the QR to stay on screen.
  Sessions live in the daemon, so closing a terminal does not kill its session;
  reconnect with 'moona attach <id>'. The phone sees all sessions as tabs.

Daemon flags:
  --host string    host/interface to bind (default 127.0.0.1)
  --port int       port to listen on (default 8787)
  --token string   optional app token; can also use MOONA_TOKEN
  --tunnel bool    start a free tunnel (Cloudflare Quick Tunnel first) (default true)
  --qr bool        print a terminal QR code for the best phone URL (default true)

Attach flags:
  --url string     daemon URL to attach to (default: local daemon)
  --session string session id (or pass it positionally: moona attach 2)
  --token string   optional app token; can also use MOONA_TOKEN

Examples:
  moona claude
  moona codex
  moona daemon
  moona ls
  moona attach 2
  moona --tunnel=false pwsh.exe -NoLogo
  moona qr`)
}
