package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	tea "github.com/charmbracelet/bubbletea"
)

// The onboarding wizard: a small bubbletea program that lets the user pick how
// their phone reaches this machine and, for the stable providers, collects the
// few settings needed to keep the URL the same across restarts. It writes
// config.json and returns whether it completed. It runs on first `moona` launch
// and any time via `moona setup`.

type obStep int

const (
	obPickProvider obStep = iota
	obNgrokToken
	obNgrokDomain
	obCFHostname
	obReview
)

type obOption struct {
	id    string
	title string
	desc  string
}

// The provider menu, in recommended order. Quick Tunnel first (works instantly),
// then the two stable options, then local-only.
var obProviders = []obOption{
	{providerQuick, "Quick Tunnel", "Instant. Nothing to set up. URL changes each restart."},
	{providerNgrok, "ngrok  —  permanent link, no domain", "Free account, paste one token. Same URL every time."},
	{providerCFNamed, "Cloudflare  —  permanent link, your domain", "Cleanest URL. Needs a domain you own on Cloudflare."},
	{providerLocal, "Local Wi-Fi only", "No public URL. Phone must be on the same network."},
}

type obModel struct {
	step   obStep
	cursor int
	cfg    userConfig
	input  string
	done   bool // completed and saved
	width  int
	height int
	err    string
}

func newObModel() obModel { return obModel{step: obPickProvider} }

func runOnboard() (userConfig, bool, error) {
	fm, err := tea.NewProgram(newObModel(), tea.WithAltScreen()).Run()
	if err != nil {
		return userConfig{}, false, err
	}
	om, _ := fm.(obModel)
	// Cloudflare is auto-provisioned after the TUI exits (it shells out to
	// cloudflared, which opens a browser and prints progress to the real terminal).
	if om.done && om.cfg.TunnelProvider == providerCFNamed {
		if perr := provisionCloudflare(&om.cfg); perr != nil {
			fmt.Println()
			fmt.Println("Cloudflare setup didn't finish:", perr)
			fmt.Println("Re-run `moona setup` to try again, or pick another option.")
		}
		// Persist any fields provisioning filled in (e.g. the default tunnel name).
		_ = writeUserConfig(om.cfg)
	}
	return om.cfg, om.done, nil
}

// maybeRunFirstRunSetup shows the setup wizard the first time moona is used (from
// any entry point), so a new user consciously picks how their phone connects
// before a session starts. Returns whether the user is now onboarded. Quitting
// the wizard is not an error (the caller decides whether to proceed anyway); only
// a hard failure to run the TUI (e.g. no TTY) returns err.
func maybeRunFirstRunSetup() (onboarded bool, err error) {
	if isOnboarded() {
		return true, nil
	}
	// The wizard is an interactive TUI; never launch it when there is no real
	// console (piped/scripted `moona claude`), or it would block. Fall through to
	// the historical Quick Tunnel default instead.
	if cols, rows := currentConsoleSize(); cols <= 0 || rows <= 0 {
		return false, nil
	}
	_, done, err := runOnboard()
	if err != nil {
		return false, err
	}
	return done, nil
}

func (m obModel) Init() tea.Cmd { return nil }

func (m obModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m obModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}

	if m.step == obPickProvider {
		switch msg.String() {
		case "q", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(obProviders)-1 {
				m.cursor++
			}
		case "enter":
			return m.chooseProvider()
		}
		return m, nil
	}

	if m.step == obReview {
		switch msg.String() {
		case "enter":
			if err := m.finalize(); err != nil {
				m.err = err.Error()
				return m, nil
			}
			m.done = true
			return m, tea.Quit
		case "esc":
			m.step = obPickProvider
			m.input = ""
			m.err = ""
			return m, nil
		}
		return m, nil
	}

	// Text-entry steps (ngrok/cloudflare fields).
	switch msg.String() {
	case "esc":
		return m.backFromInput()
	case "enter":
		return m.advanceFromInput()
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

func (m obModel) chooseProvider() (tea.Model, tea.Cmd) {
	m.cfg.TunnelProvider = obProviders[m.cursor].id
	m.input = ""
	m.err = ""
	switch m.cfg.TunnelProvider {
	case providerNgrok:
		m.step = obNgrokToken
	case providerCFNamed:
		m.step = obCFHostname
	default: // quick, local
		m.step = obReview
	}
	return m, nil
}

func (m obModel) advanceFromInput() (tea.Model, tea.Cmd) {
	v := strings.TrimSpace(m.input)
	switch m.step {
	case obNgrokToken:
		if v == "" {
			m.err = "an authtoken is required for ngrok"
			return m, nil
		}
		m.cfg.NgrokAuthtoken = v
		m.input = m.cfg.NgrokDomain
		m.err = ""
		m.step = obNgrokDomain
	case obNgrokDomain:
		// Optional: without a reserved domain ngrok assigns a random one each run.
		m.cfg.NgrokDomain = normalizeHost(v)
		m.step = obReview
	case obCFHostname:
		if v == "" {
			m.err = "the public hostname is required (e.g. moona.example.com)"
			return m, nil
		}
		m.cfg.CFHostname = normalizeHost(v)
		if m.cfg.CFTunnel == "" {
			m.cfg.CFTunnel = "moona" // moona creates/reuses a tunnel with this name
		}
		m.step = obReview
	}
	m.input = ""
	return m, nil
}

func (m obModel) backFromInput() (tea.Model, tea.Cmd) {
	switch m.step {
	case obNgrokToken, obCFHostname:
		m.step = obPickProvider
	case obNgrokDomain:
		m.input = m.cfg.NgrokAuthtoken
		m.step = obNgrokToken
	}
	m.err = ""
	return m, nil
}

// finalize fills in derived fields (an app token for public providers) and
// persists the config.
func (m *obModel) finalize() error {
	m.cfg.Onboarded = true
	if providerIsPublic(m.cfg.TunnelProvider) && m.cfg.Token == "" {
		m.cfg.Token = generateToken()
	}
	return writeUserConfig(m.cfg)
}

func normalizeHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return strings.TrimSuffix(s, "/")
}

// ----- view -----

func (m obModel) View() string {
	var body string
	switch m.step {
	case obPickProvider:
		body = m.providerView()
	case obNgrokToken:
		body = m.inputView(
			"ngrok  ·  step 1 of 2",
			"Paste your ngrok authtoken.",
			"Get it free at  dashboard.ngrok.com/get-started/your-authtoken",
			"authtoken")
	case obNgrokDomain:
		body = m.inputView(
			"ngrok  ·  step 2 of 2",
			"Paste your reserved domain (optional but recommended for a stable URL).",
			"Create one free at  dashboard.ngrok.com/cloud-edge/domains  e.g. moona-yourname.ngrok-free.app",
			"domain")
	case obCFHostname:
		body = m.inputView(
			"Cloudflare",
			"What public address should your phone use? (a subdomain of a domain you own on Cloudflare)",
			"moona will authorize Cloudflare in your browser, then create the tunnel + DNS for you.",
			"hostname")
	case obReview:
		body = m.reviewView()
	}
	panel := stPanel.Render(body)
	if m.width <= 0 || m.height <= 0 {
		return "\n" + panel
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, panel)
}

func (m obModel) providerView() string {
	lines := []string{
		stBrand.Render("◗ moona") + stDim.Render("  ·  first-time setup"),
		stDim.Render("How should your phone reach this machine?"),
		"",
	}
	for i, p := range obProviders {
		selected := i == m.cursor
		title := p.title
		if selected {
			lines = append(lines,
				stMarker.Render("❯ ")+stSel.Render(" "+title+" "))
			lines = append(lines, "    "+stDim.Render(p.desc))
		} else {
			lines = append(lines, "  "+stText.Render(title))
			lines = append(lines, "    "+stFaint.Render(p.desc))
		}
	}
	lines = append(lines, "", stFaint.Render("↑↓ move   ↵ choose   q quit  (change later: moona setup)"))
	return strings.Join(lines, "\n")
}

func (m obModel) inputView(title, prompt, hint, field string) string {
	lines := []string{
		stBrand.Render("◗ moona") + stDim.Render("  ·  "+title),
		"",
		stText.Render(prompt),
		stFaint.Render(hint),
		"",
		stDim.Render(field+": ") + stVal.Render(m.input) + stMarker.Render("▏"),
	}
	if m.err != "" {
		lines = append(lines, "", stErr.Render("• "+m.err))
	}
	lines = append(lines, "", stFaint.Render("↵ next   esc back"))
	return strings.Join(lines, "\n")
}

func (m obModel) reviewView() string {
	lines := []string{
		stBrand.Render("◗ moona") + stDim.Render("  ·  review"),
		"",
		stDim.Render("Phone access:  ") + stText.Render(providerLabel(m.cfg.TunnelProvider)),
	}
	switch m.cfg.TunnelProvider {
	case providerNgrok:
		dom := m.cfg.NgrokDomain
		if dom == "" {
			dom = stFaint.Render("(random URL each run — add a reserved domain for a stable link)")
		} else {
			dom = stVal.Render("https://" + dom)
		}
		lines = append(lines, stDim.Render("URL:           ")+dom)
		lines = append(lines, stDim.Render("Authtoken:     ")+stText.Render(maskSecret(m.cfg.NgrokAuthtoken)))
	case providerCFNamed:
		lines = append(lines, stDim.Render("URL:           ")+stVal.Render("https://"+m.cfg.CFHostname))
		lines = append(lines, "", stDim.Render("On continue, moona authorizes Cloudflare (one browser click, skipped if"))
		lines = append(lines, stDim.Render("already done) and creates the tunnel + DNS record automatically."))
	case providerQuick:
		lines = append(lines, stFaint.Render("A fresh https URL is generated each time the daemon starts."))
	case providerLocal:
		lines = append(lines, stFaint.Render("Reachable only from devices on your local network."))
	}
	if providerIsPublic(m.cfg.TunnelProvider) {
		lines = append(lines, "", stDim.Render("An app token will be generated so the link isn't an open shell."))
	}
	if m.err != "" {
		lines = append(lines, "", stErr.Render("• "+m.err))
	}
	lines = append(lines, "", stFaint.Render("↵ save & continue   esc start over"))
	return strings.Join(lines, "\n")
}

func maskSecret(s string) string {
	if len(s) <= 6 {
		return strings.Repeat("•", len(s))
	}
	return s[:3] + strings.Repeat("•", len(s)-6) + s[len(s)-3:]
}

// runSetup handles `moona setup`: runs the wizard and reports what changed.
func runSetup(_ []string) error {
	cfg, done, err := runOnboard()
	if err != nil {
		return err
	}
	if !done {
		fmt.Println("Setup cancelled — nothing changed.")
		return nil
	}
	fmt.Println("Saved. Phone access:", providerLabel(cfg.TunnelProvider))
	if cfg.TunnelProvider == providerNgrok && cfg.NgrokDomain == "" {
		fmt.Println("Note: no reserved ngrok domain set, so the URL still changes each run.")
		fmt.Println("      Add one at dashboard.ngrok.com/cloud-edge/domains, then rerun `moona setup`.")
	}
	if _, ok := daemonAlive(); ok {
		fmt.Println()
		fmt.Println("A daemon is already running with the old settings. Restart it to apply:")
		fmt.Println("  moona daemon stop      # then start work again, e.g. moona claude")
	}
	return nil
}
