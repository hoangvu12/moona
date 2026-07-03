package main

import (
	"flag"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// runUI handles bare `moona` (and `moona ui`/`dash`/`tui`): a live, keyboard-driven
// dashboard over the daemon's control API. It ensures a daemon is running, then
// renders a session list you can attach to, spawn, and kill without retyping
// commands. Attaching hands the real terminal to `moona attach` via ExecProcess
// (Bubble Tea pauses the TUI, the raw passthrough owns the console, and the TUI
// resumes when you detach), so it reuses the exact attach path a native terminal
// already uses -- ConPTY sizing and all.
func runUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	host := fs.String("host", "127.0.0.1", "host/interface to bind (when auto-starting the daemon)")
	port := fs.Int("port", defaultPort, "port to listen on (when auto-starting the daemon)")
	token := fs.String("token", os.Getenv("MOONA_TOKEN"), "optional app token; can also use MOONA_TOKEN")
	tunnel := fs.Bool("tunnel", true, "start a free tunnel when auto-starting the daemon")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := ensureDaemon(daemonOptions{host: *host, port: *port, token: *token, tunnel: *tunnel})
	if err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	m := newUIModel(st, exe)
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		return err
	}
	return nil
}

// ----- model -----

type uiMode int

const (
	modeList uiMode = iota
	modeNew
	modeConfirmKill
	modeQR
	modeHelp
)

type uiModel struct {
	api   *apiClient
	state daemonState
	exe   string

	sessions []sessionInfo
	cursor   int

	mode  uiMode
	input string // typed command in modeNew

	status string // transient one-line status / error
	width  int
	height int
}

func newUIModel(st daemonState, exe string) uiModel {
	return uiModel{
		api:   newAPIClient(st),
		state: st,
		exe:   exe,
	}
}

// ----- messages -----

type sessionsMsg struct {
	sessions []sessionInfo
	state    daemonState
	err      error
}
type tickMsg struct{}
type attachDoneMsg struct{ err error }
type createdMsg struct {
	id  string
	err error
}
type actionMsg struct {
	text string
	err  error
}

// ----- commands -----

func (m uiModel) pollCmd() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		sessions, err := api.listSessions()
		st, _ := readState() // refresh URLs (public URL appears once the tunnel is up)
		return sessionsMsg{sessions: sessions, state: st, err: err}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m uiModel) attachCmd(id string) tea.Cmd {
	c := exec.Command(m.exe, "attach", id, "--quiet")
	return tea.ExecProcess(c, func(err error) tea.Msg { return attachDoneMsg{err: err} })
}

func (m uiModel) createCmd(cmdLine string) tea.Cmd {
	api := m.api
	cols, rows := m.newSessionSize()
	cwd := mustGetwd()
	return func() tea.Msg {
		id, err := api.createSession(cmdLine, cwd, cols, rows)
		return createdMsg{id: id, err: err}
	}
}

func (m uiModel) killCmd(id string) tea.Cmd {
	api := m.api
	return func() tea.Msg {
		err := api.deleteSession(id)
		return actionMsg{text: "killed session " + id, err: err}
	}
}

// newSessionSize picks the pty size for a session started from the dashboard. We
// use the console the TUI is running in so the phone (and a later `moona attach`)
// inherit a sane geometry.
func (m uiModel) newSessionSize() (int, int) {
	if m.width > 0 && m.height > 0 {
		return m.width, m.height
	}
	if cols, rows := currentConsoleSize(); cols > 0 && rows > 0 {
		return cols, rows
	}
	return 100, 30
}

// ----- bubbletea plumbing -----

func (m uiModel) Init() tea.Cmd {
	return tea.Batch(m.pollCmd(), tickCmd())
}

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.pollCmd(), tickCmd())

	case sessionsMsg:
		if msg.err != nil {
			m.status = "list: " + msg.err.Error()
		} else {
			m.sessions = msg.sessions
			m.clampCursor()
		}
		if msg.state.LocalURL != "" {
			m.state = msg.state
		}
		return m, nil

	case attachDoneMsg:
		if msg.err != nil {
			m.status = "attach: " + msg.err.Error()
		}
		// The session may have exited while attached; refresh right away.
		return m, m.pollCmd()

	case createdMsg:
		if msg.err != nil {
			m.status = "new session: " + msg.err.Error()
			return m, nil
		}
		m.status = "started session " + msg.id
		return m, m.attachCmd(msg.id)

	case actionMsg:
		if msg.err != nil {
			m.status = msg.err.Error()
		} else {
			m.status = msg.text
		}
		return m, m.pollCmd()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m uiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Text-entry mode captures most keys for the command being typed.
	if m.mode == modeNew {
		switch msg.String() {
		case "esc":
			m.mode = modeList
			m.input = ""
		case "enter":
			cmdLine := strings.TrimSpace(m.input)
			if cmdLine == "" {
				cmdLine = defaultShellCommand()
			}
			m.mode = modeList
			m.input = ""
			return m, m.createCmd(cmdLine)
		case "backspace":
			m.input = trimLastRune(m.input)
		case "space":
			m.input += " "
		default:
			if len(msg.Runes) > 0 {
				m.input += string(msg.Runes)
			}
		}
		return m, nil
	}

	// Overlays: any key dismisses back to the list.
	if m.mode == modeQR || m.mode == modeHelp {
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		m.mode = modeList
		return m, nil
	}

	if m.mode == modeConfirmKill {
		switch msg.String() {
		case "y", "Y", "enter":
			m.mode = modeList
			if s := m.selected(); s != nil {
				return m, m.killCmd(s.ID)
			}
		default:
			m.mode = modeList
		}
		return m, nil
	}

	// modeList.
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.sessions)-1 {
			m.cursor++
		}
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = len(m.sessions) - 1
	case "enter", "a":
		if s := m.selected(); s != nil {
			m.status = ""
			return m, m.attachCmd(s.ID)
		}
		m.status = "no session to attach"
	case "n":
		m.mode = modeNew
		m.input = ""
		m.status = ""
	case "x", "d", "delete":
		if m.selected() != nil {
			m.mode = modeConfirmKill
		}
	case "u":
		m.mode = modeHelp // URLs live in the help/info overlay
	case "c":
		m.mode = modeQR
	case "r":
		return m, m.pollCmd()
	case "?":
		m.mode = modeHelp
	}
	return m, nil
}

func (m *uiModel) clampCursor() {
	if m.cursor >= len(m.sessions) {
		m.cursor = len(m.sessions) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m uiModel) selected() *sessionInfo {
	if m.cursor < 0 || m.cursor >= len(m.sessions) {
		return nil
	}
	return &m.sessions[m.cursor]
}

func trimLastRune(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(r[:len(r)-1])
}
