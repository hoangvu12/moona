package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"moona/internal/conpty"
)

// idleTimeout is how long an auto-started daemon waits with zero sessions before
// shutting itself down. Manually started daemons (moona daemon) never idle-exit,
// so the QR/dashboard stays up as long as you want it.
const idleTimeout = 15 * time.Minute

// daemonOptions configure the switchboard process.
type daemonOptions struct {
	host   string
	port   int
	token  string
	tunnel bool
	qr     bool
	auto   bool // enable idle-exit (set when auto-started by a client)
	detach bool // re-spawn self in the background and return
}

// daemonState is persisted to %LOCALAPPDATA%\moona\daemon.json so any other
// moona process can find and talk to a running daemon.
type daemonState struct {
	Port      int    `json:"port"`
	Host      string `json:"host"`
	Token     string `json:"token"`
	PID       int    `json:"pid"`
	LocalURL  string `json:"localURL"`
	PublicURL string `json:"publicURL"`
	Started   string `json:"started"`
}

func stateDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			base = dir
		}
	}
	return filepath.Join(base, appName)
}

func statePath() string { return filepath.Join(stateDir(), "daemon.json") }

func writeState(st daemonState) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(), b, 0o600)
}

func readState() (daemonState, error) {
	var st daemonState
	b, err := os.ReadFile(statePath())
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

func clearState() { _ = os.Remove(statePath()) }

// clearStateIfOwner removes the state file only if it still points at this
// process, so a daemon that lost a startup race never deletes the winner's file.
func clearStateIfOwner() {
	if st, err := readState(); err == nil && st.PID != os.Getpid() {
		return
	}
	clearState()
}

// daemonAlive reports the recorded daemon state if a daemon is actually
// answering on its recorded URL. A stale state file (crashed daemon) reads as
// not alive.
func daemonAlive() (daemonState, bool) {
	st, err := readState()
	if err != nil || st.LocalURL == "" {
		return st, false
	}
	if err := waitForShare(st.LocalURL, 600*time.Millisecond); err != nil {
		return st, false
	}
	return st, true
}

// ensureDaemon returns a live daemon, auto-starting one in the background if
// none is running. When a daemon already exists its recorded token/URL win, so a
// client always talks to the daemon that is actually up.
func ensureDaemon(opts daemonOptions) (daemonState, error) {
	if st, ok := daemonAlive(); ok {
		return st, nil
	}
	if err := spawnDetachedDaemon(opts); err != nil {
		return daemonState{}, fmt.Errorf("start moona daemon: %w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := daemonAlive(); ok {
			return st, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return daemonState{}, errors.New("moona daemon did not become ready in time (see daemon.log)")
}

// spawnDetachedDaemon launches `moona daemon --auto ...` as a background process
// that outlives this one, with output redirected to a log file.
func spawnDetachedDaemon(opts daemonOptions) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"daemon", "--auto", "--host", opts.host, "--port", strconv.Itoa(opts.port)}
	if opts.token != "" {
		args = append(args, "--token", opts.token)
	}
	if !opts.tunnel {
		args = append(args, "--tunnel=false")
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = detachedSysProcAttr()
	cmd.Stdin = nil
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	if logf, err := os.OpenFile(filepath.Join(stateDir(), "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	return cmd.Start()
}

// ----- daemon process -----

type daemon struct {
	opts     daemonOptions
	hub      *hub
	server   *http.Server
	localURL string
	errc     chan error

	// mu guards publicURL and tunnel, which the tunnel supervisor goroutine
	// mutates while state()/status handlers read them.
	mu        sync.Mutex
	publicURL string
	tunnel    *tempTunnel

	closing   atomic.Bool // set when shutting down so the supervisor stops restarting
	phoneOnce sync.Once   // fires markPhoneConnectedOnce the first time a browser attaches
}

func runDaemon(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "stop":
			return stopDaemon()
		case "status":
			return printDaemonStatus()
		}
	}

	opts, err := parseDaemonOptions(args)
	if err != nil {
		return err
	}

	// Background-spawn form: re-launch ourselves detached and return immediately.
	if opts.detach {
		if err := spawnDetachedDaemon(opts); err != nil {
			return err
		}
		st, err := ensureDaemon(opts)
		if err != nil {
			return err
		}
		fmt.Println("moona daemon started in background:", st.LocalURL)
		return nil
	}

	// If a daemon is already running, act as a dashboard: show the link + QR and
	// exit rather than fighting for the port.
	if st, ok := daemonAlive(); ok {
		fmt.Println("moona daemon already running (pid", strconv.Itoa(st.PID)+")")
		printDashboard(st.LocalURL, st.PublicURL, opts.host, st.Token, opts.qr)
		return nil
	}

	if runtime.GOOS != "windows" {
		return errors.New("this MVP is Windows-native and requires Windows ConPTY")
	}
	if !conpty.IsAvailable() {
		return conpty.ErrUnsupported
	}

	// Apply the user's saved setup (tunnel provider + app token). A never-onboarded
	// install has no config.json, so uc is zero and behavior matches the historical
	// Quick Tunnel default.
	uc, _ := readUserConfig()
	if opts.token == "" && uc.Token != "" {
		opts.token = uc.Token
	}

	d, err := startDaemonServer(opts)
	if err != nil {
		// Port already taken? Another daemon likely beat us to it (two terminals
		// racing to auto-start). Fall back to showing its dashboard rather than
		// erroring or clobbering its state file.
		if st, ok := daemonAlive(); ok {
			printDashboard(st.LocalURL, st.PublicURL, opts.host, st.Token, opts.qr)
			return nil
		}
		return err
	}
	defer d.shutdown()

	if err := waitForShare(d.localURL, 3*time.Second); err != nil {
		return err
	}

	// For fixed-hostname providers (named CF tunnel, ngrok reserved domain) the
	// public URL is known from config before the tunnel even connects, so set it
	// now — otherwise the first client reads the state below before the tunnel is
	// up and falls back to the local-only URL.
	if opts.tunnel && tunnelEnabledFor(uc.TunnelProvider) {
		d.publicURL = knownPublicURL(uc)
	}

	// Persist discovery info immediately (before the tunnel connects) so clients
	// can find us right away; update again once the live public URL is confirmed.
	_ = writeState(d.state())

	if opts.tunnel && tunnelEnabledFor(uc.TunnelProvider) {
		// Start once synchronously so the dashboard below can print the live URL,
		// then hand the process to a supervisor: tunnel helpers (cloudflared/ngrok/
		// ssh) can exit on their own — a dropped edge connection, a transient network
		// blip — while the daemon keeps running, which would silently strand the
		// public hostname (e.g. Cloudflare error 1033). The supervisor reconnects.
		tunnel, publicURL, err := startConfiguredTunnel(opts.port, uc)
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning:", err)
			// First attempt failed; drop the advertised URL but keep retrying in the
			// background so a transient failure (e.g. network not up yet) recovers.
			d.setTunnel(nil, "", true)
		} else {
			d.setTunnel(tunnel, publicURL, false)
		}
		go d.superviseTunnel(opts.port, uc, tunnel)
	}

	printDashboard(d.localURL, d.getPublicURL(), opts.host, opts.token, opts.qr)

	if opts.auto {
		go d.watchIdle()
	}
	go d.watchSignals()

	return <-d.errc
}

func parseDaemonOptions(args []string) (daemonOptions, error) {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	host := fs.String("host", "127.0.0.1", "host/interface to bind")
	port := fs.Int("port", defaultPort, "port to listen on")
	token := fs.String("token", os.Getenv("MOONA_TOKEN"), "optional app token required by clients")
	tunnel := fs.Bool("tunnel", true, "start a free temporary tunnel (Cloudflare Quick Tunnel first; auto-downloads cloudflared if needed)")
	qr := fs.Bool("qr", true, "print a terminal QR code for the best phone URL")
	auto := fs.Bool("auto", false, "enable idle-exit when no sessions are open (set automatically when auto-started)")
	detach := fs.Bool("detach", false, "start the daemon in the background and return")
	if err := fs.Parse(args); err != nil {
		return daemonOptions{}, err
	}
	return daemonOptions{
		host:   *host,
		port:   *port,
		token:  *token,
		tunnel: *tunnel,
		qr:     *qr,
		auto:   *auto,
		detach: *detach,
	}, nil
}

func startDaemonServer(opts daemonOptions) (*daemon, error) {
	// Bind synchronously so a failure (port already in use) is reported here,
	// before we write or clear any shared state.
	addr := fmt.Sprintf("%s:%d", opts.host, opts.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	d := &daemon{
		opts:     opts,
		hub:      newHub(),
		localURL: fmt.Sprintf("http://%s:%d", opts.host, opts.port),
		errc:     make(chan error, 1),
	}
	// Push the session list to every browser whenever it changes, so tab bars
	// update without polling.
	d.hub.onChange = d.pushSessions

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/ws", d.handleWS)
	mux.HandleFunc("/api/info", d.apiGuard(d.handleInfo))
	mux.HandleFunc("/api/sessions", d.apiGuard(d.handleSessions))
	mux.HandleFunc("/api/sessions/", d.apiGuard(d.handleSessionByID))
	mux.HandleFunc("/api/shutdown", d.apiGuard(d.handleShutdown))

	d.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := d.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.errc <- err
			return
		}
		d.errc <- nil
	}()
	return d, nil
}

func (d *daemon) state() daemonState {
	return daemonState{
		Port:      d.opts.port,
		Host:      d.opts.host,
		Token:     d.opts.token,
		PID:       os.Getpid(),
		LocalURL:  d.localURL,
		PublicURL: d.getPublicURL(),
		Started:   nowStamp(),
	}
}

// getPublicURL reads the current public URL under the lock (the supervisor
// goroutine may be updating it).
func (d *daemon) getPublicURL() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.publicURL
}

// setTunnel records the current tunnel process + public URL under the lock and
// persists discovery state. clear drops the advertised URL (the tunnel is down,
// so the host would only answer with an error); otherwise a non-empty publicURL
// replaces the current one and an empty one leaves it untouched (keeping a
// pre-known fixed hostname in place).
func (d *daemon) setTunnel(t *tempTunnel, publicURL string, clear bool) {
	d.mu.Lock()
	d.tunnel = t
	switch {
	case clear:
		d.publicURL = ""
	case publicURL != "":
		d.publicURL = publicURL
	}
	d.mu.Unlock()
	_ = writeState(d.state())
}

// superviseTunnel keeps the configured tunnel alive for the daemon's lifetime.
// It is seeded with the process from the initial (synchronous) start; whenever
// that process exits it reconnects with capped backoff, so a dropped tunnel never
// leaves the public hostname stranded while the daemon keeps serving.
func (d *daemon) superviseTunnel(port int, uc userConfig, current *tempTunnel) {
	backoff := time.Second
	for {
		if current == nil {
			if d.closing.Load() {
				return
			}
			tunnel, publicURL, err := startConfiguredTunnel(port, uc)
			if err != nil {
				if d.closing.Load() {
					return
				}
				log.Printf("tunnel: %v; reconnecting in %s", err, backoff.Round(time.Second))
				time.Sleep(backoff)
				if backoff < 30*time.Second {
					backoff *= 2
				}
				continue
			}
			backoff = time.Second
			d.setTunnel(tunnel, publicURL, false)
			current = tunnel
		}
		if current == nil {
			return // provider produced no process (only happens for local-only)
		}
		current.wait() // block until the tunnel helper process exits
		current = nil
		if d.closing.Load() {
			return
		}
		log.Printf("tunnel connection dropped; reconnecting")
	}
}

func (d *daemon) shutdown() {
	d.closing.Store(true) // stop the supervisor from restarting the tunnel we kill
	d.mu.Lock()
	t := d.tunnel
	d.mu.Unlock()
	if t != nil {
		t.stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.server.Shutdown(ctx)
	d.hub.closeAll()
	clearStateIfOwner()
}

func (d *daemon) watchIdle() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if d.hub.idleFor(idleTimeout) {
			log.Printf("no sessions for %s; shutting down idle daemon", idleTimeout)
			select {
			case d.errc <- nil:
			default:
			}
			return
		}
	}
}

func (d *daemon) watchSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
	select {
	case d.errc <- nil:
	default:
	}
}

// ----- HTTP handlers -----

func (d *daemon) tokenOK(r *http.Request) bool {
	if d.opts.token == "" {
		return true
	}
	if r.URL.Query().Get("token") == d.opts.token {
		return true
	}
	return r.Header.Get("X-Moona-Token") == d.opts.token
}

func (d *daemon) apiGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.tokenOK(r) {
			http.Error(w, "missing or invalid token", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (d *daemon) handleWS(w http.ResponseWriter, r *http.Request) {
	if !d.tokenOK(r) {
		http.Error(w, "missing or invalid token", http.StatusUnauthorized)
		return
	}
	id := r.URL.Query().Get("session")
	sess := d.hub.get(id)
	if sess == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}
	c := &client{
		conn: conn,
		send: make(chan []byte, 64),
		// Native terminals (moona attach / the launching console) connect with
		// primary=1 and become the size authority; browsers conform.
		primary: r.URL.Query().Get("primary") == "1",
	}
	// The first time an actual phone/browser connects, remember it so `moona claude`
	// stops showing the connect panel on every start.
	if !c.primary {
		d.phoneOnce.Do(markPhoneConnectedOnce)
	}
	sess.attach(c)
	// Seed this browser's tab bar with the current session list; subsequent
	// changes arrive via the onChange broadcast.
	c.queue(d.sessionsMsg())
	go writePump(c)
	// Make the running program redraw its current frame so this freshly attached
	// (and reset) client shows a clean screen instead of nothing.
	sess.forceRepaint()
	readPump(sess, c)
	sess.detach(c)
	_ = conn.Close()
}

func (d *daemon) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"localURL":  d.localURL,
		"publicURL": d.getPublicURL(),
		"pid":       os.Getpid(),
		"sessions":  d.hub.list(),
	})
}

func (d *daemon) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, d.hub.list())
	case http.MethodPost:
		var req struct {
			Cmd  string `json:"cmd"`
			Cwd  string `json:"cwd"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		cmd := strings.TrimSpace(req.Cmd)
		if cmd == "" {
			cmd = defaultShellCommand()
		}
		cwd := req.Cwd
		if cwd == "" {
			cwd = mustGetwd()
		}
		cols, rows := req.Cols, req.Rows
		if cols <= 0 {
			cols = 100
		}
		if rows <= 0 {
			rows = 30
		}
		cfg := config{
			host:        d.opts.host,
			port:        d.opts.port,
			commandLine: cmd,
			workDir:     cwd,
			token:       d.opts.token,
			cols:        cols,
			rows:        rows,
		}
		sess, err := d.hub.createSession(cfg)
		if err != nil {
			http.Error(w, "start session: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"id": sess.id})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (d *daemon) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.Trim(id, "/")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.hub.get(id) == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	d.hub.remove(id)
	w.WriteHeader(http.StatusNoContent)
}

func (d *daemon) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	go func() {
		time.Sleep(100 * time.Millisecond)
		select {
		case d.errc <- nil:
		default:
		}
	}()
}

// sessionsMsg builds a {type:"sessions"} frame carrying the current session list.
func (d *daemon) sessionsMsg() []byte {
	return mustJSON(wsMessage{Type: "sessions", Sessions: d.hub.list()})
}

// pushSessions broadcasts the current session list to every connected browser.
func (d *daemon) pushSessions() { d.hub.broadcast(d.sessionsMsg()) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
