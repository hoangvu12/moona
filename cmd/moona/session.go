package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/gorilla/websocket"

	"moona/internal/conpty"
)

type wsMessage struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Message string `json:"message,omitempty"`
	// Sessions carries the daemon's current session list on a {type:"sessions"}
	// frame, so browsers refresh their tab bar from a push instead of polling.
	Sessions []sessionInfo `json:"sessions,omitempty"`
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

	// id and command are set by the hub when the session is registered. command
	// is the display string shown in `moona ls` and the browser tab bar.
	id      string
	command string
	// onExit is invoked once when the underlying process ends, so the hub can
	// drop the session from its registry. Set by the hub before the ConPTY starts.
	onExit func()

	mu      sync.Mutex
	pty     *conpty.ConPty
	running bool
	closed  bool
	clients map[*client]struct{}
	buffer  *ringBuffer
	// Effective (smallest-client) size last applied to the ConPTY. Broadcast to
	// clients so a larger client can clear ghost output when the shared render
	// width changes because another client joined or left.
	lastEffCols int
	lastEffRows int
}

func newSession(cfg config) *session {
	return &session{
		cfg:     cfg,
		command: cfg.commandLine,
		clients: make(map[*client]struct{}),
		buffer:  newRingBuffer(512 * 1024),
	}
}

func (s *session) ensureStarted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running && s.pty != nil {
		return nil
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

// snapshot returns a point-in-time view of the session for the control API and
// the browser tab bar.
func (s *session) snapshot() sessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	cols, rows := s.lastEffCols, s.lastEffRows
	if cols <= 0 {
		cols = s.cfg.cols
	}
	if rows <= 0 {
		rows = s.cfg.rows
	}
	return sessionInfo{
		ID:      s.id,
		Command: s.command,
		Cols:    cols,
		Rows:    rows,
		Clients: len(s.clients),
		Running: s.running,
	}
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
	onExit := s.onExit
	s.mu.Unlock()

	msg := mustJSON(wsMessage{Type: "status", Message: message})
	for _, c := range clients {
		c.queue(msg)
	}
	// Let the hub drop us from its registry so the session disappears from
	// `moona ls` and the browser tab bar once the program has exited.
	if onExit != nil {
		onExit()
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

// queueToAll queues msg to every client currently attached to this session.
func (s *session) queueToAll(msg []byte) {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.queue(msg)
	}
}

func (c *client) queue(msg []byte) {
	select {
	case c.send <- msg:
	default:
	}
}

func mustJSON(msg wsMessage) []byte {
	b, err := json.Marshal(msg)
	if err != nil {
		panic(err)
	}
	return b
}
