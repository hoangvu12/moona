package main

import (
	"net/url"
	"os"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

func printPhoneQR(running *runningShare) {
	if !running.cfg.qr {
		return
	}
	target := running.publicURL
	label := "Scan on phone"
	if target == "" {
		if isLoopbackHost(running.cfg.host) {
			return
		}
		target = running.localURL
		label = "Scan on phone (same network only)"
	}
	openURL := browserURL(target, running.cfg.token)
	terminalPrintln(os.Stdout, label+":")
	terminalPrintln(os.Stdout, openURL)
	if running.cfg.token != "" {
		terminalPrintln(os.Stdout, "(QR includes the app token.)")
	}
	terminalPrintf(os.Stdout, "%s", renderTerminalQR(openURL))
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
