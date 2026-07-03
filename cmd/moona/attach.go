package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// runAttach handles `moona attach [id] [flags]`. With no id it attaches to the
// only running session (or lists them if there is more than one). It talks to
// the local daemon by default, or a remote one via --url.
func runAttach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	endpoint := fs.String("url", "", "daemon URL to attach to (default: local daemon)")
	token := fs.String("token", os.Getenv("MOONA_TOKEN"), "optional app token; can also use MOONA_TOKEN")
	sessionID := fs.String("session", "", "session id to attach (see `moona ls`)")
	quiet := fs.Bool("quiet", false, "suppress local attach status text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Allow `moona attach 2` as shorthand for `--session 2`.
	if *sessionID == "" && fs.NArg() > 0 {
		*sessionID = fs.Arg(0)
	}

	base, tok := *endpoint, *token
	if base == "" {
		st, ok := daemonAlive()
		if !ok {
			return errors.New("moona daemon is not running; start a session with, e.g., `moona claude`")
		}
		base = st.LocalURL
		if tok == "" {
			tok = st.Token
		}
	}

	id := *sessionID
	if id == "" {
		resolved, err := resolveSingleSession(base, tok)
		if err != nil {
			return err
		}
		id = resolved
	}

	wsURL, err := sessionWebSocketURL(base, tok, id, true)
	if err != nil {
		return err
	}
	return attachToWebSocketURL(wsURL, *quiet)
}

// attachSession attaches this terminal to a specific session on a known daemon.
func attachSession(st daemonState, id string, quiet bool) error {
	wsURL, err := sessionWebSocketURL(st.LocalURL, st.Token, id, true)
	if err != nil {
		return err
	}
	return attachToWebSocketURL(wsURL, quiet)
}

// resolveSingleSession picks the session to attach when none was named: the sole
// session if there is exactly one, otherwise an error listing the choices.
func resolveSingleSession(base, token string) (string, error) {
	c := &apiClient{base: strings.TrimRight(base, "/"), token: token}
	sessions, err := c.listSessions()
	if err != nil {
		return "", err
	}
	switch len(sessions) {
	case 0:
		return "", errors.New("no active sessions; start one with, e.g., `moona claude`")
	case 1:
		return sessions[0].ID, nil
	default:
		var b strings.Builder
		b.WriteString("multiple sessions; pick one with `moona attach <id>`:\n")
		for _, s := range sessions {
			fmt.Fprintf(&b, "  %s  %s\n", s.ID, s.Command)
		}
		return "", errors.New(b.String())
	}
}

func sessionWebSocketURL(base, token, sessionID string, primary bool) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = fmt.Sprintf("http://127.0.0.1:%d", defaultPort)
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
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
	u.Path = "/ws"
	q := u.Query()
	if sessionID != "" {
		q.Set("session", sessionID)
	}
	// A native terminal is the size authority — mark it so the daemon pins the
	// ConPTY to it and lets browsers conform rather than shrink it.
	if primary {
		q.Set("primary", "1")
	}
	if token != "" {
		q.Set("token", token)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func attachToWebSocketURL(wsURL string, quiet bool) error {
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
	writeJSONMsg := func(m wsMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(m)
	}

	if cols > 0 && rows > 0 {
		_ = writeJSONMsg(wsMessage{Type: "resize", Cols: cols, Rows: rows})
	}
	if quiet {
		clearTerminalScreen(os.Stdout)
	} else {
		fmt.Fprintf(os.Stderr, "attached to %s\r\n", wsURL)
		fmt.Fprintln(os.Stderr, "Ctrl-C is sent to the remote terminal. Press Ctrl-] to detach; the session keeps running.\r")
	}

	done := make(chan error, 3)
	stop := make(chan struct{})
	defer close(stop)
	go attachReceiveLoop(conn, done)
	go attachInputLoop(writeJSONMsg, done)
	// Track live console-window resizes (Windows has no SIGWINCH) so the native
	// terminal stays the authoritative size and browsers follow it.
	go attachResizeLoop(writeJSONMsg, stop, done)

	err = <-done
	writeMu.Lock()
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	writeMu.Unlock()
	if errors.Is(err, errDetached) {
		if !quiet {
			fmt.Fprintln(os.Stderr, "\r\n[moona] detached; the session keeps running.\r")
		}
		return nil
	}
	if err != nil && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// errDetached signals that the user pressed the detach key (Ctrl-]) rather than
// the remote program exiting. attachToWebSocketURL treats it as a clean return so
// the caller (e.g. the dashboard TUI, or a plain `moona attach`) resumes without
// killing the session.
var errDetached = errors.New("detached")

// detachByte is Ctrl-] (0x1d) -- the classic telnet escape, chosen because it is
// almost never used by shells or full-screen TUIs, so forwarding everything else
// raw stays safe.
const detachByte = 0x1d

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

func attachInputLoop(writeJSONMsg func(wsMessage) error, done chan<- error) {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			// Detach on Ctrl-]: forward any input that preceded it, then stop the
			// passthrough cleanly (the session stays alive in the daemon).
			if i := indexByte(buf[:n], detachByte); i >= 0 {
				if i > 0 {
					_ = writeJSONMsg(wsMessage{Type: "input", Data: string(buf[:i])})
				}
				done <- errDetached
				return
			}
			if writeErr := writeJSONMsg(wsMessage{Type: "input", Data: string(buf[:n])}); writeErr != nil {
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

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// attachResizeLoop polls the local console size and forwards changes to the
// session so resizing the native terminal window updates the ConPTY (and every
// browser that conforms to it). Windows has no SIGWINCH, so we poll.
func attachResizeLoop(writeJSONMsg func(wsMessage) error, stop <-chan struct{}, done chan<- error) {
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
			if err := writeJSONMsg(wsMessage{Type: "resize", Cols: cols, Rows: rows}); err != nil {
				select {
				case done <- err:
				default:
				}
				return
			}
		}
	}
}
