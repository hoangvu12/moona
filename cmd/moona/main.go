package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/gorilla/websocket"
	qrcode "github.com/skip2/go-qrcode"

	"moona/internal/conpty"
)

const appName = "moona"
const defaultPort = 8787

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
	startPaused bool
}

type wsMessage struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Message string `json:"message,omitempty"`
}

type client struct {
	conn *websocket.Conn
	send chan []byte
	// Last size this client reported. The shared ConPTY has a single size, so
	// these are combined (see effectiveClientSizeLocked) rather than applied directly.
	cols int
	rows int
	// primary marks a native terminal (moona attach / the launching console).
	// A native terminal's grid cannot be resized by moona, so it must be the
	// size AUTHORITY: the ConPTY tracks it, and browsers conform to it. Browser
	// clients (primary=false) never shrink the ConPTY; they letterbox/scroll.
	primary bool
}

type session struct {
	cfg config

	mu           sync.Mutex
	pty          *conpty.ConPty
	running      bool
	closed       bool
	startAllowed bool
	clients      map[*client]struct{}
	buffer       *ringBuffer
	// Effective (smallest-client) size last applied to the ConPTY. Broadcast to
	// clients so a larger client can clear ghost output when the shared render
	// width changes because another client joined or left.
	lastEffCols int
	lastEffRows int
}

func newSession(cfg config) *session {
	return &session{
		cfg:          cfg,
		startAllowed: !cfg.startPaused,
		clients:      make(map[*client]struct{}),
		buffer:       newRingBuffer(512 * 1024),
	}
}

func (s *session) ensureStarted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running && s.pty != nil {
		return nil
	}
	if !s.startAllowed {
		return errTerminalStartPending
	}
	if !conpty.IsAvailable() {
		return conpty.ErrUnsupported
	}
	pty, err := conpty.Start(conpty.Options{
		CommandLine: s.cfg.commandLine,
		WorkDir:     s.cfg.workDir,
		Cols:        s.cfg.cols,
		Rows:        s.cfg.rows,
	})
	if err != nil {
		return err
	}
	s.pty = pty
	s.running = true
	s.closed = false
	s.buffer.Reset()
	go s.readLoop(pty)
	go s.waitLoop(pty)
	return nil
}

var errTerminalStartPending = errors.New("terminal start is waiting for local confirmation")

func (s *session) allowStart() {
	s.mu.Lock()
	s.startAllowed = true
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	msg := mustJSON(wsMessage{Type: "status", Message: "starting terminal"})
	for _, c := range clients {
		c.queue(msg)
	}
}

func (s *session) attach(c *client) {
	s.mu.Lock()
	s.clients[c] = struct{}{}
	effCols, effRows := s.lastEffCols, s.lastEffRows
	s.mu.Unlock()

	// Do NOT replay the raw output ring buffer. For a full-screen TUI (Claude
	// Code, etc.) it is a stream of cursor-positioned frame updates; replaying it
	// from an arbitrary midpoint into a fresh terminal produces garbled scrollback
	// -- the symptom seen when simply opening the page, no second client needed.
	// Instead reset the client, then force the app to repaint its current frame
	// (see forceRepaint in handleWS) so the new client gets a clean screen.
	c.queue(mustJSON(wsMessage{Type: "reset"}))
	// Tell the new client the width the app is currently rendering at, so it can
	// match/ignore ghost output instead of assuming its own fitted size.
	if effCols > 0 && effRows > 0 {
		c.queue(mustJSON(wsMessage{Type: "size", Cols: effCols, Rows: effRows}))
	}
	c.queue(mustJSON(wsMessage{Type: "status", Message: "connected"}))
}

func (s *session) detach(c *client) {
	s.mu.Lock()
	delete(s.clients, c)
	pty := s.pty
	running := s.running
	cols, rows := s.minClientSizeLocked()
	msg, clients := s.effectiveSizeBroadcastLocked(cols, rows)
	s.mu.Unlock()
	close(c.send)
	// A client leaving may lift the size constraint (e.g. the phone disconnects,
	// so the desktop can reclaim its full width). Recompute, resize, and tell the
	// remaining clients so they clear ghost output from the previous width.
	if running && pty != nil && cols > 0 && rows > 0 {
		_ = pty.Resize(cols, rows)
		for _, cl := range clients {
			cl.queue(msg)
		}
	}
}

func (s *session) writeInput(data string) error {
	s.mu.Lock()
	pty := s.pty
	running := s.running
	s.mu.Unlock()
	if !running || pty == nil {
		return errors.New("terminal session is not running")
	}
	_, err := pty.Write([]byte(data))
	return err
}

func (s *session) resize(c *client, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	s.mu.Lock()
	c.cols = cols
	c.rows = rows
	pty := s.pty
	running := s.running
	effCols, effRows := s.minClientSizeLocked()
	msg, clients := s.effectiveSizeBroadcastLocked(effCols, effRows)
	s.mu.Unlock()
	if !running || pty == nil || effCols <= 0 || effRows <= 0 {
		return nil
	}
	if err := pty.Resize(effCols, effRows); err != nil {
		return err
	}
	// If the effective width changed, tell every client so larger ones can clear
	// ghost output left over from the previous width.
	for _, cl := range clients {
		cl.queue(msg)
	}
	return nil
}

// forceRepaint briefly jiggles the pseudoconsole size so the running program
// receives a resize (SIGWINCH-equivalent) and redraws its current frame. This is
// how a newly attached client gets a clean, correct screen without replaying the
// raw scrollback. It is a no-op if the terminal is not running.
func (s *session) forceRepaint() {
	s.mu.Lock()
	pty := s.pty
	running := s.running
	cols, rows := s.lastEffCols, s.lastEffRows
	s.mu.Unlock()
	if !running || pty == nil {
		return
	}
	if cols <= 0 {
		cols = s.cfg.cols
	}
	if rows <= 0 {
		rows = s.cfg.rows
	}
	if cols <= 0 || rows <= 0 {
		return
	}
	_ = pty.Resize(cols, rows+1)
	_ = pty.Resize(cols, rows)
}

// effectiveSizeBroadcastLocked records a new effective size and, if it changed,
// returns a "size" message plus the clients to notify. Returns nil when the size
// is unchanged or invalid, so callers can loop over the (possibly empty) client
// slice unconditionally. Caller must hold s.mu.
func (s *session) effectiveSizeBroadcastLocked(cols, rows int) ([]byte, []*client) {
	if cols <= 0 || rows <= 0 || (cols == s.lastEffCols && rows == s.lastEffRows) {
		return nil, nil
	}
	s.lastEffCols = cols
	s.lastEffRows = rows
	clients := make([]*client, 0, len(s.clients))
	for cl := range s.clients {
		clients = append(clients, cl)
	}
	return mustJSON(wsMessage{Type: "size", Cols: cols, Rows: rows}), clients
}

// minClientSizeLocked returns the smallest cols/rows across all clients that
// have reported a size. The phone browser and the desktop terminal share one
// ConPTY, which can only hold a single size; sizing it to the minimum keeps the
// entire logical screen visible on every client (tmux's default policy) instead
// of letting the last resize win and garbling the larger client's TUI. Clients
// that have not reported a size yet (cols/rows 0) are ignored. Caller holds s.mu.
func (s *session) minClientSizeLocked() (int, int) {
	// A native terminal (primary) cannot be resized by moona, so if any is
	// attached it is the size AUTHORITY: the ConPTY tracks the primaries and
	// browser sizes are ignored entirely (browsers conform + scroll/zoom, never
	// shrink the ConPTY). This keeps the original terminal's grid fixed no matter
	// what device connects. With several primaries we take the smallest so every
	// native terminal stays within the ConPTY. Only when NO primary is present do
	// browsers drive the size (phone-only use, where there is nothing to obey).
	cols, rows := s.minSizeLocked(true)
	if cols > 0 && rows > 0 {
		return cols, rows
	}
	return s.minSizeLocked(false)
}

// minSizeLocked returns the smallest reported size across clients. When
// primaryOnly is true, only native-terminal clients are considered. Clients
// that have not reported a size yet (cols/rows 0) are ignored. Caller holds s.mu.
func (s *session) minSizeLocked(primaryOnly bool) (int, int) {
	cols, rows := 0, 0
	for cl := range s.clients {
		if primaryOnly && !cl.primary {
			continue
		}
		if cl.cols <= 0 || cl.rows <= 0 {
			continue
		}
		if cols == 0 || cl.cols < cols {
			cols = cl.cols
		}
		if rows == 0 || cl.rows < rows {
			rows = cl.rows
		}
	}
	return cols, rows
}

func (s *session) readLoop(pty *conpty.ConPty) {
	buf := make([]byte, 32*1024)
	for {
		n, err := pty.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.buffer.Write(chunk)
			clients := make([]*client, 0, len(s.clients))
			for c := range s.clients {
				clients = append(clients, c)
			}
			s.mu.Unlock()
			msg := mustJSON(wsMessage{Type: "output", Data: string(chunk)})
			for _, c := range clients {
				c.queue(msg)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("terminal read ended: %v", err)
			}
			s.finishTerminal(pty, "terminal exited")

			return
		}
	}
}

func (s *session) waitLoop(pty *conpty.ConPty) {
	exitCode, err := pty.Wait(context.Background())
	if err != nil {
		log.Printf("terminal wait ended: %v", err)
	}
	s.finishTerminal(pty, fmt.Sprintf("terminal exited with code %d", exitCode))
}

func (s *session) finishTerminal(pty *conpty.ConPty, message string) {
	s.mu.Lock()
	if s.pty != pty {
		s.mu.Unlock()
		return
	}
	s.running = false
	s.pty = nil
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	msg := mustJSON(wsMessage{Type: "status", Message: message})
	for _, c := range clients {
		c.queue(msg)
	}
}

func (s *session) close() {
	s.mu.Lock()
	pty := s.pty
	s.pty = nil
	s.running = false
	s.closed = true
	s.mu.Unlock()
	if pty != nil {
		_ = pty.Close()
	}
}

func (c *client) queue(msg []byte) {
	select {
	case c.send <- msg:
	default:
	}
}

type ringBuffer struct {
	mu   sync.Mutex
	data []byte
	cap  int
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{cap: capacity}
}

func (r *ringBuffer) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= r.cap {
		r.data = append(r.data[:0], p[len(p)-r.cap:]...)
		return
	}
	r.data = append(r.data, p...)
	if overflow := len(r.data) - r.cap; overflow > 0 {
		copy(r.data, r.data[overflow:])
		r.data = r.data[:r.cap]
	}
}

func (r *ringBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.data...)
}

func (r *ringBuffer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = r.data[:0]
}

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
		return runShare(args)
	}
	switch args[0] {
	case "share", "serve":
		return runShare(args[1:])
	case "attach":
		return runAttach(args[1:])
	case "help", "-h", "--help":
		printHelp()
		return nil
	case "version", "--version":
		fmt.Println("moona dev")
		return nil
	default:
		return runShortcut(args)
	}
}
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

type runningShare struct {
	cfg       config
	sess      *session
	server    *http.Server
	localURL  string
	publicURL string
	tunnel    *tempTunnel
	errc      chan error
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
		startPaused: true,
		tunnel:      *tunnel,
	}, nil
}

func startShareServer(cfg config) (*runningShare, error) {
	if runtime.GOOS != "windows" {
		return nil, errors.New("this MVP is Windows-native and requires Windows ConPTY")
	}
	if !conpty.IsAvailable() {
		return nil, conpty.ErrUnsupported
	}

	sess := newSession(cfg)
	if !cfg.startPaused {
		if err := sess.ensureStarted(); err != nil {
			return nil, fmt.Errorf("start terminal: %w", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/ws", sess.handleWS(cfg))

	addr := fmt.Sprintf("%s:%d", cfg.host, cfg.port)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	running := &runningShare{
		cfg:      cfg,
		sess:     sess,
		server:   server,
		localURL: "http://" + addr,
		errc:     make(chan error, 1),
	}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			running.errc <- err
			return
		}
		running.errc <- nil
	}()
	return running, nil
}

func (r *runningShare) shutdown() {
	if r.tunnel != nil {
		r.tunnel.stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.server.Shutdown(ctx)
	r.sess.close()
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

type tempTunnel struct {
	cmd *exec.Cmd
}

func maybeStartTunnel(running *runningShare) error {
	if !running.cfg.tunnel {
		return nil
	}

	var failures []string
	for _, provider := range tunnelProviders(running.cfg.port) {
		terminalPrintln(os.Stderr, "starting temporary tunnel via ", provider.name, "...")
		tunnel, publicURL, err := startTunnelProvider(provider, 45*time.Second)
		if err != nil {
			failures = append(failures, provider.name+": "+err.Error())
			continue
		}
		running.tunnel = tunnel
		running.publicURL = publicURL
		terminalPrintln(os.Stderr, "tunnel ready via ", provider.name)
		return nil
	}

	return fmt.Errorf("all tunnel providers failed: %s", strings.Join(failures, " | "))
}

type tunnelProvider struct {
	name    string
	binary  string
	args    []string
	prepare func() (string, error)
}

func tunnelProviders(port int) []tunnelProvider {
	return []tunnelProvider{
		{
			name:    "Cloudflare Quick Tunnel",
			binary:  "cloudflared",
			prepare: ensureCloudflared,
			args:    []string{"tunnel", "--url", fmt.Sprintf("http://127.0.0.1:%d", port)},
		},
		{
			name:   "Pinggy",
			binary: "ssh",
			args: []string{
				"-p", "443",
				"-o", "BatchMode=yes",
				"-o", "NumberOfPasswordPrompts=0",
				"-o", "ExitOnForwardFailure=yes",
				"-o", "StrictHostKeyChecking=accept-new",
				"-o", "ServerAliveInterval=30",
				"-R", fmt.Sprintf("0:127.0.0.1:%d", port),
				"qr@free.pinggy.io",
			},
		},
		{
			name:   "localhost.run",
			binary: "ssh",
			args: []string{
				"-o", "ExitOnForwardFailure=yes",
				"-o", "StrictHostKeyChecking=accept-new",
				"-o", "ServerAliveInterval=30",
				"-R", fmt.Sprintf("80:127.0.0.1:%d", port),
				"nokey@localhost.run",
			},
		},
	}
}
func resolveTunnelBinary(provider tunnelProvider) (string, error) {
	if provider.prepare != nil {
		return provider.prepare()
	}
	binaryPath, err := exec.LookPath(provider.binary)
	if err != nil {
		return "", fmt.Errorf("%s not found in PATH", provider.binary)
	}
	return binaryPath, nil
}

func ensureCloudflared() (string, error) {
	if cloudflaredPath, err := exec.LookPath("cloudflared"); err == nil {
		return cloudflaredPath, nil
	}
	if runtime.GOOS != "windows" {
		return "", errors.New("cloudflared not found in PATH")
	}

	cacheDir, err := os.UserCacheDir()
	if err != nil || cacheDir == "" {
		cacheDir = os.Getenv("LOCALAPPDATA")
	}
	if cacheDir == "" {
		return "", errors.New("cannot find a cache directory for cloudflared")
	}

	targetDir := filepath.Join(cacheDir, appName)
	targetPath := filepath.Join(targetDir, "cloudflared.exe")
	if info, err := os.Stat(targetPath); err == nil && !info.IsDir() {
		return targetPath, nil
	}

	downloadURL, err := cloudflaredDownloadURL()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", err
	}

	terminalPrintln(os.Stderr, "cloudflared not found; downloading Cloudflare Quick Tunnel helper...")
	tmpPath := targetPath + ".tmp"
	if err := downloadFile(downloadURL, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	_ = os.Chmod(targetPath, 0o755)
	return targetPath, nil
}

func cloudflaredDownloadURL() (string, error) {
	suffix := runtime.GOARCH
	switch runtime.GOARCH {
	case "amd64", "arm64", "386":
		// Cloudflare uses Go architecture names in the Windows release assets.
	default:
		return "", fmt.Errorf("cloudflared auto-download is unsupported on %s", runtime.GOARCH)
	}
	return "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-" + suffix + ".exe", nil
}

func downloadFile(downloadURL, targetPath string) error {
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(downloadURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download %s failed: %s", downloadURL, resp.Status)
	}

	out, err := os.Create(targetPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func startTunnelProvider(provider tunnelProvider, timeout time.Duration) (*tempTunnel, string, error) {
	binaryPath, err := resolveTunnelBinary(provider)
	if err != nil {
		return nil, "", err
	}

	cmd := exec.Command(binaryPath, provider.args...)
	cmd.Stdin = strings.NewReader("\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", err
	}

	var recentOutput []string
	lines := make(chan string, 32)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}

	scan := func(r io.Reader) {
		s := bufio.NewScanner(r)
		for s.Scan() {
			select {
			case lines <- s.Text():
			default:
			}
		}
	}
	go scan(stdout)
	go scan(stderr)
	go func() { done <- cmd.Wait() }()

	tunnel := &tempTunnel{cmd: cmd}
	urlRE := regexp.MustCompile(`https?://[^\s\]"'<>]+`)
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case line := <-lines:
			line = strings.TrimSpace(line)
			if line != "" {
				recentOutput = appendRecent(recentOutput, line, 8)
			}
			if match := findTunnelURL(line, urlRE); match != "" {
				return tunnel, match, nil
			}
		case err := <-done:
			if err == nil {
				err = errors.New("tunnel exited before printing a URL")
			}
			return nil, "", fmt.Errorf("%w%s", err, formatRecentTunnelOutput(recentOutput))
		case <-timer.C:
			tunnel.stop()
			return nil, "", fmt.Errorf("timed out waiting for tunnel URL after %s%s", timeout.Round(time.Second), formatRecentTunnelOutput(recentOutput))
		}
	}
}

func findTunnelURL(line string, urlRE *regexp.Regexp) string {
	if line == "" {
		return ""
	}
	lowerLine := strings.ToLower(line)
	matches := urlRE.FindAllString(line, -1)
	for _, match := range matches {
		match = strings.TrimRight(match, ".,;)]}")
		u, err := url.Parse(match)
		if err != nil {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if host == "" || host == "twitter.com" || host == "localhost.run" || host == "admin.localhost.run" || host == "pinggy.io" || host == "developers.cloudflare.com" {
			continue
		}
		switch {
		case strings.HasSuffix(host, ".pinggy.link"):
			return match
		case strings.HasSuffix(host, ".trycloudflare.com"):
			return match
		case strings.HasSuffix(host, ".lhr.life"):
			return match
		case strings.Contains(lowerLine, "tunneled with tls termination") && strings.HasSuffix(host, ".localhost.run"):
			return match
		}
	}
	return ""
}
func appendRecent(lines []string, line string, limit int) []string {
	lines = append(lines, line)
	if len(lines) > limit {
		copy(lines, lines[len(lines)-limit:])
		lines = lines[:limit]
	}
	return lines
}

func formatRecentTunnelOutput(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return "; last output: " + strings.Join(lines, " | ")
}

func (t *tempTunnel) stop() {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return
	}
	_ = t.cmd.Process.Kill()
	_, _ = t.cmd.Process.Wait()
}

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
func waitForShare(localURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := http.Client{Timeout: 200 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get(strings.TrimRight(localURL, "/") + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("server did not become ready at %s", localURL)
}

func runAttach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	endpoint := fs.String("url", fmt.Sprintf("http://127.0.0.1:%d", defaultPort), "moona share URL to attach to")
	token := fs.String("token", os.Getenv("MOONA_TOKEN"), "optional app token; can also use MOONA_TOKEN")
	quiet := fs.Bool("quiet", false, "suppress local attach status text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	wsURL, err := attachWebSocketURL(*endpoint, *token)
	if err != nil {
		return err
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", wsURL, err)
	}
	defer conn.Close()

	restore, cols, rows, err := prepareLocalTerminal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not enable raw console mode:", err)
	} else {
		defer restore()
	}
	// gorilla/websocket allows only one concurrent writer. The input loop, the
	// size poller, and the close handshake all write, so funnel them through one
	// serialized helper.
	var writeMu sync.Mutex
	writeJSON := func(m wsMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(m)
	}

	if cols > 0 && rows > 0 {
		_ = writeJSON(wsMessage{Type: "resize", Cols: cols, Rows: rows})
	}
	if *quiet {
		clearTerminalScreen(os.Stdout)
	} else {
		fmt.Fprintf(os.Stderr, "attached to %s\r\n", wsURL)
		fmt.Fprintln(os.Stderr, "Ctrl-C is sent to the remote terminal. Close this window or stop moona share to detach.\r")
	}

	done := make(chan error, 3)
	stop := make(chan struct{})
	defer close(stop)
	go attachReceiveLoop(conn, done)
	go attachInputLoop(writeJSON, done)
	// Track live console-window resizes (Windows has no SIGWINCH) so the native
	// terminal stays the authoritative size and browsers follow it.
	go attachResizeLoop(writeJSON, stop, done)

	err = <-done
	writeMu.Lock()
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	writeMu.Unlock()
	if err != nil && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func attachWebSocketURL(endpoint, token string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = fmt.Sprintf("http://127.0.0.1:%d", defaultPort)
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/ws"
	}
	// A native terminal is the size authority — mark it so the server pins the
	// ConPTY to it and lets browsers conform rather than shrink it.
	q := u.Query()
	q.Set("primary", "1")
	if token != "" && q.Get("token") == "" {
		q.Set("token", token)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func attachReceiveLoop(conn *websocket.Conn, done chan<- error) {
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			done <- err
			return
		}
		var msg wsMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "output":
			_, _ = os.Stdout.Write([]byte(msg.Data))
		case "status":
			if msg.Message != "" && msg.Message != "connected" {
				fmt.Fprintf(os.Stderr, "\r\n[moona] %s\r\n", msg.Message)
				if strings.HasPrefix(msg.Message, "terminal exited") {
					done <- nil
					return
				}
			}
		case "error":
			if msg.Message != "" {
				fmt.Fprintf(os.Stderr, "\r\n[moona] error: %s\r\n", msg.Message)
			}
		}
	}
}

func attachInputLoop(writeJSON func(wsMessage) error, done chan<- error) {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if writeErr := writeJSON(wsMessage{Type: "input", Data: string(buf[:n])}); writeErr != nil {
				done <- writeErr
				return
			}
		}
		if err != nil {
			done <- err
			return
		}
	}
}

// attachResizeLoop polls the local console size and forwards changes to the
// session so resizing the native terminal window updates the ConPTY (and every
// browser that conforms to it). Windows has no SIGWINCH, so we poll.
func attachResizeLoop(writeJSON func(wsMessage) error, stop <-chan struct{}, done chan<- error) {
	lastCols, lastRows := 0, 0
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			cols, rows := currentConsoleSize()
			if cols <= 0 || rows <= 0 || (cols == lastCols && rows == lastRows) {
				continue
			}
			lastCols, lastRows = cols, rows
			if err := writeJSON(wsMessage{Type: "resize", Cols: cols, Rows: rows}); err != nil {
				select {
				case done <- err:
				default:
				}
				return
			}
		}
	}
}

func (s *session) handleWS(cfg config) http.HandlerFunc {
	upgrader := websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true
			}
			u, err := url.Parse(origin)
			return err == nil && strings.EqualFold(u.Host, r.Host)
		},
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.token != "" && r.URL.Query().Get("token") != cfg.token {
			http.Error(w, "missing or invalid token", http.StatusUnauthorized)
			return
		}
		startPending := false
		if err := s.ensureStarted(); err != nil {
			if errors.Is(err, errTerminalStartPending) {
				startPending = true
			} else {
				http.Error(w, "terminal unavailable: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade failed: %v", err)
			return
		}
		c := &client{
			conn: conn,
			send: make(chan []byte, 64),
			// Native terminals (moona attach) connect with primary=1 and become
			// the size authority; browsers connect without it and conform.
			primary: r.URL.Query().Get("primary") == "1",
		}
		s.attach(c)
		if startPending {
			c.queue(mustJSON(wsMessage{Type: "status", Message: "waiting for local confirmation before starting terminal"}))
		}
		go writePump(c)
		// Make the running program redraw its current frame so this freshly
		// attached (and reset) client shows a clean screen instead of nothing.
		s.forceRepaint()
		readPump(s, c)
		s.detach(c)
		_ = conn.Close()
	}
}

func readPump(s *session, c *client) {
	defer c.conn.Close()
	c.conn.SetReadLimit(1 << 20)
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	})
	for {
		_, payload, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg wsMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "input":
			if err := s.writeInput(msg.Data); err != nil {
				c.queue(mustJSON(wsMessage{Type: "error", Message: err.Error()}))
			}
		case "resize":
			if err := s.resize(c, msg.Cols, msg.Rows); err != nil {
				c.queue(mustJSON(wsMessage{Type: "error", Message: err.Error()}))
			}
		case "ping":
			c.queue(mustJSON(wsMessage{Type: "status", Message: "pong"}))
		}
	}
}

func writePump(c *client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	defer c.conn.Close()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(indexHTML))
}

func mustJSON(msg wsMessage) []byte {
	b, err := json.Marshal(msg)
	if err != nil {
		panic(err)
	}
	return b
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

func printHelp() {
	fmt.Println(`Moona - Windows-native web terminal for phone access

Usage:
  moona <command> [args...]       show link/QR, wait, then start and attach
  moona codex                    shortcut example with Enter/q confirmation
  moona share [flags]
  moona share -- codex
  moona share --cmd "pwsh.exe"
  moona attach [flags]

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

// utf16Encode is intentionally unused right now, but kept as a tiny compile-time guard
// against accidental non-Windows command-line assumptions while this MVP evolves.
var _ = utf16.Encode

const indexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover" />
  <title>Moona Terminal</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/xterm@5.3.0/css/xterm.css" />
  <style>
    :root { color-scheme: dark; --bar: #111827; --line: #243042; --text: #d7e0ee; --muted: #95a3b8; }
    * { box-sizing: border-box; }
    html, body { width: 100%; height: 100%; margin: 0; overflow: hidden; background: #05070a; color: var(--text); font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    body { display: flex; flex-direction: column; }
    #topbar { min-height: 42px; display: flex; align-items: center; gap: 8px; padding: 6px 8px calc(6px + env(safe-area-inset-top)); background: var(--bar); border-bottom: 1px solid var(--line); }
    #brand { font-weight: 700; letter-spacing: .02em; }
    #status { color: var(--muted); font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    /* The grid is pinned to the native terminal's size (not the device's), so on
       a narrow phone it can be wider than the screen. Allow horizontal panning;
       xterm keeps vertical scroll via its own .xterm-viewport. Pinch-zoom also
       works (viewport meta permits user scaling). */
    #terminal-wrap { flex: 1; min-height: 0; padding: 8px 8px 4px; overflow-x: auto; overflow-y: hidden; -webkit-overflow-scrolling: touch; }
    #terminal { width: 100%; height: 100%; }
    #keys { display: flex; gap: 6px; overflow-x: auto; padding: 6px 8px calc(8px + env(safe-area-inset-bottom)); background: var(--bar); border-top: 1px solid var(--line); -webkit-overflow-scrolling: touch; }
    button { flex: 0 0 auto; border: 1px solid #334155; background: #182235; color: var(--text); padding: 9px 12px; border-radius: 10px; font-weight: 650; font-size: 13px; }
    button:active { transform: translateY(1px); background: #24324b; }
    .xterm { height: 100%; }
    .xterm .xterm-viewport { overflow-y: auto; }
    @media (max-width: 640px) {
      #topbar { min-height: 38px; }
      #brand { font-size: 13px; }
      button { padding: 9px 11px; }
      #terminal-wrap { padding: 6px 6px 2px; }
    }
  </style>
</head>
<body>
  <div id="topbar"><span id="brand">Moona</span><span id="status">loading assets...</span></div>
  <div id="terminal-wrap"><div id="terminal"></div></div>
  <div id="keys">
    <button data-send="\u001b">Esc</button>
    <button data-send="\t">Tab</button>
    <button data-send="\u0003">Ctrl-C</button>
    <button data-send="\u0004">Ctrl-D</button>
    <button data-send="\r">Enter</button>
    <button data-send="\u001b[A">↑</button>
    <button data-send="\u001b[B">↓</button>
    <button data-send="\u001b[D">←</button>
    <button data-send="\u001b[C">→</button>
    <button id="paste">Paste</button>
  </div>

  <script src="https://cdn.jsdelivr.net/npm/xterm@5.3.0/lib/xterm.js"></script>
  <script src="https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.8.0/lib/xterm-addon-fit.js"></script>
  <script>
    const statusEl = document.getElementById('status');
    const params = new URLSearchParams(location.search);
    const token = params.get('token') || '';
    const term = new Terminal({
      cursorBlink: true,
      // NOTE: no convertEol. The ConPTY stream carries its own CR/LF and cursor
      // positioning; rewriting \n to \r\n (as convertEol does) mangles a
      // repainting TUI's output over time. The real console (moona attach)
      // renders the same bytes cleanly precisely because it does not do this.
      fontFamily: 'Cascadia Mono, Consolas, Menlo, Monaco, monospace',
      fontSize: window.innerWidth < 640 ? 13 : 14,
      theme: { background: '#05070a', foreground: '#d7e0ee', cursor: '#f8fafc', selectionBackground: '#334155' },
      allowProposedApi: true
    });
    const fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open(document.getElementById('terminal'));
    term.focus();

    let ws = null;
    let reconnectTimer = null;
    let resizeTimer = null;

    function setStatus(text) { statusEl.textContent = text; }
    function wsURL() {
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const suffix = token ? ('?token=' + encodeURIComponent(token)) : '';
      return proto + '//' + location.host + '/ws' + suffix;
    }
    function send(msg) {
      if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
    }
    function sendInput(data) { send({ type: 'input', data }); }
    // The size we last REQUESTED from the server (our natural fit). This is only
    // a request: the server picks the effective size (min across all clients)
    // and echoes it back via a {type:'size'} message, which is the ONLY thing
    // that actually resizes our grid. See the size handler below.
    let lastReqCols = 0, lastReqRows = 0;
    // The effective grid the server told us the app is rendering at. xterm's
    // grid is kept EXACTLY equal to this at all times, because Claude Code's
    // renderer emits purely cursor-relative deltas against a screen whose
    // dimensions == the ConPTY size. If our grid diverges from the ConPTY even
    // by one row, every incremental frame's cursor math drifts and the output
    // rots "after a moment". So the server is authoritative on grid size.
    let lastEffCols = 0, lastEffRows = 0;
    function fitAndResize() {
      // Measure our natural fit WITHOUT mutating the grid (proposeDimensions
      // does not resize; fitAddon.fit() would, and that must never race the
      // server's authoritative size). Send it as a request only.
      let dims;
      try { dims = fitAddon.proposeDimensions(); } catch (e) {}
      if (!dims || !dims.cols || !dims.rows) return;
      if (dims.cols === lastReqCols && dims.rows === lastReqRows) return;
      lastReqCols = dims.cols;
      lastReqRows = dims.rows;
      send({ type: 'resize', cols: dims.cols, rows: dims.rows });
    }
    function scheduleResize() {
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(fitAndResize, 80);
    }
    function connect() {
      clearTimeout(reconnectTimer);
      setStatus('connecting...');
      ws = new WebSocket(wsURL());
      ws.onopen = () => { setStatus('connected'); lastReqCols = 0; lastReqRows = 0; lastEffCols = 0; lastEffRows = 0; fitAndResize(); term.focus(); };
      ws.onmessage = (event) => {
        try {
          const msg = JSON.parse(event.data);
          if (msg.type === 'output') term.write(msg.data || '');
          else if (msg.type === 'reset') {
            // Server dropped the raw replay; start from a clean slate and let the
            // app repaint the current frame. Reset trackers so we re-request.
            term.reset();
            lastReqCols = 0; lastReqRows = 0; lastEffCols = 0; lastEffRows = 0;
          }
          else if (msg.type === 'size') {
            // Authoritative grid size from the server (min across all clients).
            // Force our xterm grid to EXACTLY match the ConPTY so Claude Code's
            // cursor-relative deltas stay locked and don't drift over time.
            if (msg.cols && msg.rows && (msg.cols !== lastEffCols || msg.rows !== lastEffRows)) {
              const widthChanged = lastEffCols !== 0 && msg.cols !== lastEffCols;
              term.resize(msg.cols, msg.rows);
              // A repainting TUI writes to the main screen, so old frames sit in
              // scrollback. On a WIDTH change xterm reflows that scrollback and
              // garbles it (the frames were never soft-wrapped). Drop it; the app
              // repaints the current frame. Height-only changes keep column layout
              // intact, so we preserve history to avoid wiping it on mobile scroll.
              if (widthChanged) term.clear();
              lastEffCols = msg.cols;
              lastEffRows = msg.rows;
            }
          }
          else if (msg.type === 'status') setStatus(msg.message || 'connected');
          else if (msg.type === 'error') { setStatus(msg.message || 'error'); term.writeln('\r\n[moona] ' + (msg.message || 'error')); }
        } catch (e) {}
      };
      ws.onclose = () => { setStatus('disconnected; reconnecting...'); reconnectTimer = setTimeout(connect, 1200); };
      ws.onerror = () => { setStatus('connection error'); };
    }

    term.onData(sendInput);
    window.addEventListener('resize', scheduleResize);
    window.addEventListener('orientationchange', scheduleResize);
    document.querySelectorAll('button[data-send]').forEach(btn => btn.addEventListener('click', () => { sendInput(btn.getAttribute('data-send')); term.focus(); }));
    document.getElementById('paste').addEventListener('click', async () => {
      try { sendInput(await navigator.clipboard.readText()); setStatus('pasted'); }
      catch (e) { setStatus('clipboard blocked by browser'); }
      term.focus();
    });

    fitAndResize();
    connect();
  </script>
</body>
</html>`
