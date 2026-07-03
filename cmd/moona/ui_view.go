package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// A small, cohesive palette (Catppuccin Mocha) so the dashboard reads as one
// designed surface instead of raw ANSI. lipgloss auto-degrades the hex colors on
// terminals without truecolor.
var (
	cAccent = lipgloss.Color("#CBA6F7") // mauve  — brand + focus
	cText   = lipgloss.Color("#CDD6F4") // primary text
	cDim    = lipgloss.Color("#7F849C") // secondary text
	cFaint  = lipgloss.Color("#494D64") // hairlines / separators
	cGreen  = lipgloss.Color("#A6E3A1")
	cRed    = lipgloss.Color("#F38BA8")
	cBlue   = lipgloss.Color("#89B4FA")
	cSelBg  = lipgloss.Color("#313244") // surface0 — focus fill
)

var (
	stBrand  = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	stText   = lipgloss.NewStyle().Foreground(cText)
	stDim    = lipgloss.NewStyle().Foreground(cDim)
	stFaint  = lipgloss.NewStyle().Foreground(cFaint)
	stColHdr = lipgloss.NewStyle().Bold(true).Foreground(cDim)
	stKey    = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	stVal    = lipgloss.NewStyle().Foreground(cBlue)
	stGreen  = lipgloss.NewStyle().Foreground(cGreen)
	stRed    = lipgloss.NewStyle().Foreground(cRed)
	stMarker = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	stSel    = lipgloss.NewStyle().Bold(true).Foreground(cText).Background(cSelBg)
	stErr    = lipgloss.NewStyle().Foreground(cRed)

	stPanel = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cFaint).
		Padding(1, 3)
)

// column widths inside the card (COMMAND flexes to fill)
const (
	colID     = 4
	colClient = 7
	colSize   = 9
	colState  = 10
	colGap    = 2
	colMarker = 2
)

func (m uiModel) innerWidth() int {
	w := m.width - 8 // border(2) + padding(6)
	if w < 46 {
		w = 46
	}
	if w > 96 {
		w = 96
	}
	return w
}

func (m uiModel) View() string {
	switch m.mode {
	case modeQR:
		return m.center(stPanel.Render(m.qrView()))
	case modeHelp:
		return m.center(stPanel.Render(m.helpView()))
	}

	inner := m.innerWidth()
	var lines []string
	lines = append(lines, m.brandLine(inner), m.linksLine(inner), "")
	lines = append(lines, m.tableHeader(inner), stFaint.Render(strings.Repeat("─", inner)))
	lines = append(lines, m.tableRows(inner)...)
	lines = append(lines, stFaint.Render(strings.Repeat("─", inner)), m.footerLine(inner))

	card := stPanel.Render(strings.Join(lines, "\n"))
	return "\n" + card
}

func (m uiModel) center(s string) string {
	if m.width <= 0 || m.height <= 0 {
		return s
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, s)
}

func (m uiModel) brandLine(w int) string {
	left := stBrand.Render("◗ moona") + "  " + stDim.Render("session manager")
	count := stDim.Render(fmt.Sprintf("%d session%s", len(m.sessions), plural(len(m.sessions))))
	return padBetween(left, count, w)
}

func (m uiModel) linksLine(w int) string {
	local := stDim.Render("local ") + stVal.Render(compactURL(m.state.LocalURL))
	public := stDim.Render("public ")
	if m.state.PublicURL != "" {
		public += stVal.Render(compactURL(m.state.PublicURL))
	} else {
		public += stFaint.Render("— no tunnel")
	}
	return padBetween(local, public, w)
}

func (m uiModel) tableHeader(w int) string {
	cmdW := m.commandWidth(w)
	row := fmt.Sprintf("%-*s%s%-*s%s%-*s%s%-*s%s%-*s",
		colID, "ID", gap(), cmdW, "COMMAND", gap(),
		colClient, "CLIENTS", gap(), colSize, "SIZE", gap(), colState, "STATE")
	return "  " + stColHdr.Render(row)
}

func (m uiModel) tableRows(w int) []string {
	if len(m.sessions) == 0 {
		msg := stDim.Render("No sessions yet.  Press ") + stKey.Render("n") + stDim.Render(" to start one.")
		// keep the empty state on the card at a stable height
		return []string{"", "  " + msg, ""}
	}
	cmdW := m.commandWidth(w)
	rows := make([]string, 0, len(m.sessions))
	for i, s := range m.sessions {
		rows = append(rows, m.renderRow(s, i == m.cursor, cmdW, w))
	}
	return rows
}

func (m uiModel) renderRow(s sessionInfo, selected bool, cmdW, w int) string {
	id := fmt.Sprintf("%-*s", colID, s.ID)
	cmd := fmt.Sprintf("%-*s", cmdW, truncate(s.Command, cmdW))
	clients := fmt.Sprintf("%-*d", colClient, s.Clients)
	size := fmt.Sprintf("%-*s", colSize, fmt.Sprintf("%dx%d", s.Cols, s.Rows))
	word := "running"
	if !s.Running {
		word = "exited"
	}

	if selected {
		body := fmt.Sprintf("%s%s%s%s%s%s%s%s%s %-*s",
			id, gap(), cmd, gap(), clients, gap(), size, gap(), "●", colState-2, word)
		bodyW := w - colMarker
		return stMarker.Render("❯ ") + stSel.Width(bodyW).Render(body)
	}

	dot := stGreen
	if !s.Running {
		dot = stRed
	}
	state := dot.Render("●") + " " + stDim.Render(fmt.Sprintf("%-*s", colState-2, word))
	line := stDim.Render(id) + gap() + stText.Render(cmd) + gap() +
		stDim.Render(clients) + gap() + stDim.Render(size) + gap() + state
	return "  " + line
}

func (m uiModel) footerLine(w int) string {
	switch m.mode {
	case modeNew:
		prompt := stKey.Render("new session") + stDim.Render("  (blank = default shell)")
		field := stText.Render(m.input) + stMarker.Render("▏")
		return "  " + prompt + "\n  " + field + "\n  " + stFaint.Render("enter") + stDim.Render(" run   ") +
			stFaint.Render("esc") + stDim.Render(" cancel")
	case modeConfirmKill:
		id := ""
		if s := m.selected(); s != nil {
			id = s.ID
		}
		return "  " + stErr.Render("kill session "+id+"?  ") +
			stKey.Render("y") + stDim.Render(" confirm    any other key cancels")
	}

	keys := [][2]string{
		{"↑↓", "move"}, {"↵", "attach"}, {"n", "new"}, {"x", "kill"},
		{"c", "qr"}, {"?", "help"}, {"r", "refresh"}, {"q", "quit"},
	}
	parts := make([]string, len(keys))
	for i, kv := range keys {
		parts[i] = stKey.Render(kv[0]) + " " + stDim.Render(kv[1])
	}
	bar := "  " + strings.Join(parts, stFaint.Render("   "))
	if m.status != "" {
		bar += "\n  " + stFaint.Render("› ") + stDim.Render(m.status)
	}
	return bar
}

func (m uiModel) commandWidth(w int) int {
	fixed := colMarker + colID + colClient + colSize + colState + 4*colGap
	cw := w - fixed
	if cw < 12 {
		cw = 12
	}
	return cw
}

// ----- overlays -----

func (m uiModel) qrView() string {
	target := m.state.PublicURL
	label := "Scan on your phone"
	if target == "" {
		target = m.state.LocalURL
		label = "Scan on your phone (same network only)"
	}
	qr := strings.ReplaceAll(renderTerminalQR(browserURL(target, m.state.Token)), "\r\n", "\n")
	return strings.Join([]string{
		stBrand.Render("◗ moona"),
		stDim.Render(label),
		"",
		qr,
		stVal.Render(compactURL(target)),
		"",
		stFaint.Render("press any key to go back"),
	}, "\n")
}

func (m uiModel) helpView() string {
	row := func(k, d string) string { return stKey.Render(fmt.Sprintf("%-10s", k)) + stDim.Render(d) }
	return strings.Join([]string{
		stBrand.Render("◗ moona") + stDim.Render("  keys & links"),
		"",
		stColHdr.Render("LINKS"),
		stDim.Render("local   ") + stVal.Render(compactURL(m.state.LocalURL)),
		stDim.Render("public  ") + stVal.Render(publicOrNone(m.state)),
		"",
		stColHdr.Render("KEYS"),
		row("↑ ↓ / j k", "move between sessions"),
		row("↵ / a", "attach  (Ctrl-] detaches, session keeps running)"),
		row("n", "new session (attaches on start)"),
		row("x / d", "kill the selected session"),
		row("c", "show the phone QR code"),
		row("r", "refresh now (auto every second)"),
		row("q", "quit dashboard (daemon & sessions keep running)"),
		"",
		stFaint.Render("press any key to go back"),
	}, "\n")
}

// ----- helpers -----

func gap() string { return strings.Repeat(" ", colGap) }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// padBetween left-justifies a and right-justifies b within width w, measuring by
// visible cells so ANSI styling does not throw the spacing off.
func padBetween(a, b string, w int) string {
	space := w - lipgloss.Width(a) - lipgloss.Width(b)
	if space < 1 {
		space = 1
	}
	return a + strings.Repeat(" ", space) + b
}

func compactURL(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	return strings.TrimSuffix(u, "/")
}

func publicOrNone(st daemonState) string {
	if st.PublicURL == "" {
		return "— no tunnel"
	}
	return compactURL(st.PublicURL)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
