package main

import (
	"fmt"
	"io"
	"strings"
)

func terminalPrintln(w io.Writer, args ...any) {
	line := strings.TrimSuffix(fmt.Sprintln(args...), "\n")
	_, _ = fmt.Fprint(w, line+"\r\n")
}

func terminalPrintf(w io.Writer, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\n", "\r\n")
	_, _ = fmt.Fprint(w, text)
}

func clearTerminalScreen(w io.Writer) {
	// Clear the visible viewport, scrollback, and return the cursor home so the
	// attached terminal starts from a clean screen after the QR/confirmation flow.
	_, _ = fmt.Fprint(w, "\x1b[2J\x1b[3J\x1b[H")
}
