package main

import (
	"net/url"
	"os"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// printDashboard prints the link(s), an optional QR code, and usage hints. It is
// the single "control panel" view shown when you start `moona daemon`, re-run it
// while one is already up, or call `moona qr`.
func printDashboard(localURL, publicURL, host, token string, qr bool) {
	terminalPrintln(os.Stdout, "Moona daemon is running")
	terminalPrintln(os.Stdout, "Local:  ", browserURL(localURL, token))
	if publicURL != "" {
		terminalPrintln(os.Stdout, "Public: ", browserURL(publicURL, token))
	} else {
		terminalPrintln(os.Stdout, "Public:  (no tunnel; local network / --tunnel to enable)")
	}
	if token != "" {
		terminalPrintln(os.Stdout, "Token:   enabled; links above include it")
	}
	terminalPrintln(os.Stdout)

	if qr {
		target := publicURL
		label := "Scan on phone"
		if target == "" {
			if !isLoopbackHost(host) {
				target = localURL
				label = "Scan on phone (same network only)"
			}
		}
		if target != "" {
			openURL := browserURL(target, token)
			terminalPrintln(os.Stdout, label+":")
			terminalPrintf(os.Stdout, "%s", renderTerminalQR(openURL))
			terminalPrintln(os.Stdout)
		}
	}

	terminalPrintln(os.Stdout, "Start a session in another tab (opens instantly, no prompt):")
	terminalPrintln(os.Stdout, "  moona claude          moona codex          moona pwsh.exe")
	terminalPrintln(os.Stdout, "List / manage sessions:")
	terminalPrintln(os.Stdout, "  moona ls              moona attach <id>    moona kill <id>")
	terminalPrintln(os.Stdout)
	terminalPrintln(os.Stdout, "The phone web page shows every session as a switchable tab.")
	terminalPrintln(os.Stdout, "Stop the daemon with `moona daemon stop` (or Ctrl+C here).")
}

func browserURL(base, token string) string {
	if token == "" {
		return base
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String()
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func renderTerminalQR(target string) string {
	qr, err := qrcode.New(target, qrcode.Medium)
	if err != nil {
		return "(could not render QR: " + err.Error() + ")"
	}
	bitmap := qr.Bitmap()
	const quiet = 4
	size := len(bitmap) + quiet*2
	var b strings.Builder

	moduleAt := func(x, y int) bool {
		x -= quiet
		y -= quiet
		return y >= 0 && y < len(bitmap) && x >= 0 && x < len(bitmap[y]) && bitmap[y][x]
	}

	for y := 0; y < size; y += 2 {
		b.WriteString("\x1b[47;30m")
		for x := 0; x < size; x++ {
			top := moduleAt(x, y)
			bottom := y+1 < size && moduleAt(x, y+1)
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteString("\x1b[0m\r\n")
	}
	return b.String()
}
