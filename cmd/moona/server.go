package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"moona/internal/conpty"
)

type runningShare struct {
	cfg       config
	sess      *session
	server    *http.Server
	localURL  string
	publicURL string
	tunnel    *tempTunnel
	errc      chan error
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
