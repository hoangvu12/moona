package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
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

// waitForShare polls the /healthz endpoint until the server answers or the
// timeout elapses. Used both after starting the local server and when probing
// whether a daemon recorded in the state file is actually alive.
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
	return &waitError{url: localURL}
}

type waitError struct{ url string }

func (e *waitError) Error() string { return "server did not become ready at " + e.url }

func readPump(h *hub, s *session, c *client) {
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
		case "active":
			// The user focused/backgrounded this browser tab. A focused browser
			// becomes the ConPTY size authority so the terminal follows the screen
			// you're actually on (see setActive / minClientSizeLocked).
			if err := s.setActive(c, msg.Active); err != nil {
				c.queue(mustJSON(wsMessage{Type: "error", Message: err.Error()}))
			}
		case "keepalive":
			// This page still has these sessions open as tabs. Refresh their idle
			// clocks so the ones it isn't currently viewing (no socket of their own)
			// are not reaped while the page that shows them stays open.
			h.touch(msg.KeepAlive)
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
