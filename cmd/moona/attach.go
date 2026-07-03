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
