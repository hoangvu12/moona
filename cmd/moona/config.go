package main

import (
	"errors"
	"flag"
	"os"
	"os/exec"
	"strings"
)

type config struct {
	host        string
	port        int
	commandLine string
	workDir     string
	token       string
	cols        int
	rows        int
	qr          bool
	tunnel      bool
}

func parseShareConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	host := fs.String("host", "127.0.0.1", "host/interface to bind")
	port := fs.Int("port", defaultPort, "port to listen on")
	cmdFlag := fs.String("cmd", "", "raw command line to run inside the Windows pseudoconsole")
	cwd := fs.String("cwd", mustGetwd(), "working directory for the terminal process")
	token := fs.String("token", os.Getenv("MOONA_TOKEN"), "optional app token required by websocket clients")
	cols := fs.Int("cols", 100, "initial terminal columns")
	qr := fs.Bool("qr", true, "print a terminal QR code for the best phone URL")
	tunnel := fs.Bool("tunnel", true, "start a free temporary tunnel (Cloudflare Quick Tunnel first; auto-downloads cloudflared if needed); use --tunnel=false for local-only")
	rows := fs.Int("rows", 30, "initial terminal rows")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	commandLine := strings.TrimSpace(*cmdFlag)
	if commandLine == "" {
		extra := fs.Args()
		if len(extra) > 0 {
			commandLine = quoteCommandArgs(extra)
		} else {
			commandLine = defaultShellCommand()
		}
	}

	return config{
		host:        *host,
		port:        *port,
		commandLine: commandLine,
		workDir:     *cwd,
		token:       *token,
		cols:        *cols,
		qr:          *qr,
		tunnel:      *tunnel,
		rows:        *rows,
	}, nil
}

func parseShortcutConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("shortcut", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	host := fs.String("host", "127.0.0.1", "host/interface to bind")
	port := fs.Int("port", defaultPort, "port to listen on")
	cwd := fs.String("cwd", mustGetwd(), "working directory for the terminal process")
	token := fs.String("token", os.Getenv("MOONA_TOKEN"), "optional app token required by websocket clients")
	cols := fs.Int("cols", 100, "initial terminal columns")
	rows := fs.Int("rows", 30, "initial terminal rows")
	qr := fs.Bool("qr", true, "print a terminal QR code for the best phone URL")
	tunnel := fs.Bool("tunnel", true, "start a free temporary tunnel (Cloudflare Quick Tunnel first; auto-downloads cloudflared if needed); use --tunnel=false for local-only")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	commandArgs := fs.Args()
	if len(commandArgs) == 0 {
		return config{}, errors.New("shortcut needs a command, for example: moona codex")
	}
	return config{
		host:        *host,
		port:        *port,
		commandLine: quoteCommandArgs(commandArgs),
		workDir:     *cwd,
		token:       *token,
		cols:        *cols,
		rows:        *rows,
		qr:          *qr,
		tunnel:      *tunnel,
	}, nil
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func defaultShellCommand() string {
	for _, candidate := range []string{"pwsh.exe", "powershell.exe", "cmd.exe"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return quoteArg(path)
		}
	}
	return "powershell.exe"
}

func quoteCommandArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = quoteArg(arg)
	}
	return strings.Join(quoted, " ")
}

func quoteArg(arg string) string {
	if arg == "" {
		return `""`
	}
	needsQuotes := strings.ContainsAny(arg, " \t\n\v\"")
	if !needsQuotes {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	backslashes := 0
	for _, r := range arg {
		switch r {
		case '\\':
			backslashes++
		case '"':
			b.WriteString(strings.Repeat("\\", backslashes*2+1))
			b.WriteRune(r)
			backslashes = 0
		default:
			if backslashes > 0 {
				b.WriteString(strings.Repeat("\\", backslashes))
				backslashes = 0
			}
			b.WriteRune(r)
		}
	}
	if backslashes > 0 {
		b.WriteString(strings.Repeat("\\", backslashes*2))
	}
	b.WriteByte('"')
	return b.String()
}
