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
	case "share", "serve":
		maybeAutoUpdate(args)
		return runShare(args[1:])
	case "attach":
		return runAttach(args[1:])
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
  moona <command> [args...]       show link/QR, wait, then start and attach
  moona codex                    shortcut example with Enter/q confirmation
  moona share [flags]
  moona share -- codex
  moona share --cmd "pwsh.exe"
  moona attach [flags]
  moona update [--check]           update to the latest release (auto on startup)
  moona version

Attach flags:
  --url string     moona share URL to attach to (default http://127.0.0.1:8787)
  --token string   optional app token; can also use MOONA_TOKEN

Flags:
  --host string    host/interface to bind (default 127.0.0.1)
  --port int       port to listen on (default 8787)
  --cmd string     raw command line to run in the ConPTY session
  --cwd string     working directory for the terminal process
  --token string   optional app token; can also use MOONA_TOKEN
  --cols int       initial terminal columns
  --rows int       initial terminal rows
  --qr bool        print a terminal QR code for the best phone URL (default true)
  --tunnel bool    start free tunnel via Cloudflare Quick Tunnel; auto-downloads cloudflared if needed (default true)

Examples:
  moona codex
  moona opencode
  moona claude
  moona --tunnel=false codex
  moona pwsh.exe -NoLogo
  moona share
  moona share --tunnel=false
  moona attach
  moona share -- codex
  moona share --cmd "powershell.exe -NoLogo"
  moona share --host 127.0.0.1 --port 8787
  moona attach --url http://127.0.0.1:8787

Shortcut mode prints URL/QR first, then waits for Enter to start the command or q to quit. Tunnel + QR are on by default. Tunnel order: Cloudflare Quick Tunnel, Pinggy without password prompts, localhost.run. Use --tunnel=false for local-only.`)
}
