package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"moona/internal/conpty"
)

// sessionBufferBytes is how much raw ConPTY output each session retains for
// replay to a newly attached browser. A browser client has no scrollback of its
// own (it only ever received the live stream), so this buffer is the history a
// phone sees when it scrolls up. Sized generously so it comfortably exceeds a
// typical native-console scrollback; it is gzip-compressed before sending, so the
// on-the-wire cost of a large value is small.
const sessionBufferBytes = 8 << 20 // 8 MiB

type wsMessage struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Message string `json:"message,omitempty"`
	// Enc marks how Data is encoded when it is not a plain UTF-8 terminal string.
	// Used by the {type:"replay"} history frame, whose Data is base64(gzip(bytes)).
	Enc string `json:"enc,omitempty"`
	// Active carries a browser client's focus state on a {type:"active"} frame:
	// true when the user is looking at that tab (document visible), false when it
	// is backgrounded. A focused browser becomes the ConPTY size authority so the
	// terminal follows whichever screen you're actually using. See setActive.
	Active bool `json:"active,omitempty"`
	// Sessions carries the daemon's current session list on a {type:"sessions"}
	// frame, so browsers refresh their tab bar from a push instead of polling.
	Sessions []sessionInfo `json:"sessions,omitempty"`
	// KeepAlive carries the session ids a browser page currently shows as tabs, on a
	// {type:"keepalive"} frame it sends periodically over its one active socket. The
	// daemon refreshes those sessions' idle clocks so background tabs (which have no
	// socket of their own) survive as long as a page that lists them is open. See
	// hub.touch / the idle-session reaper.
	KeepAlive []string `json:"keepalive,omitempty"`
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
	// active marks a browser client the user is currently looking at (its tab is
	// visible/focused). A single PTY can only hold one size, so the ConPTY can't
	// be both phone-narrow and desktop-wide at once; instead it FOLLOWS focus — a
	// focused browser outranks even a primary native terminal as the size
	// authority, so the terminal fits whichever screen you've switched to. When no
	// browser is focused, authority reverts to the native terminal. Set via the
	// {type:"active"} frame; only meaningful for browser (non-primary) clients.
	active bool
}

type session struct {
	cfg config

	// id and command are set by the hub when the session is registered. command
	// is the display string shown in `moona ls` and the browser tab bar.
	id      string
	command string
	// title is the live terminal window title, parsed out of the ConPTY output
	// stream (OSC 0/2 sequences). Programs like Claude Code and the shell update
	// it over time to reflect what they are doing, so the browser tab bar mirrors
	// it (falling back to command until a title arrives). Guarded by mu.
	title string
	// onExit is invoked once when the underlying process ends, so the hub can
	// drop the session from its registry. Set by the hub before the ConPTY starts.
	onExit func()
	// onTitleChange is invoked (off-lock) when title changes value, so the hub can
	// push a fresh session list to browsers. Set by the hub; throttled there.
	onTitleChange func()
	// Incremental OSC-title parser state, carried across read chunks (a title
	// sequence can straddle a Read boundary). See scanTitleLocked.
	tScan    int
	tPs      []byte
	tIsTitle bool
	tBuf     []byte

	mu      sync.Mutex
	pty     *conpty.ConPty
	running bool
	closed  bool
	clients map[*client]struct{}
	buffer  *ringBuffer
	// lastSeen is when this session last had a reason to stay alive with no client
	// attached: the moment its last client detached, or the last keepalive from a
	// browser page that still lists it as a tab. The idle-session reaper closes a
	// session that has had zero clients AND no keepalive for longer than the grace
	// window (see reapable / hub.reapIdle). Seeded to creation time so the brief
	// gap between spawning a session and its launching terminal attaching does not
	// count as abandonment. Guarded by mu.
	lastSeen time.Time
	// Effective (smallest-client) size last applied to the ConPTY. Broadcast to
	// clients so a larger client can clear ghost output when the shared render
	// width changes because another client joined or left.
	lastEffCols int
	lastEffRows int
}

func newSession(cfg config) *session {
	return &session{
		cfg:      cfg,
		command:  cfg.commandLine,
		clients:  make(map[*client]struct{}),
		buffer:   newRingBuffer(sessionBufferBytes),
		lastSeen: time.Now(),
	}
}

// markSeen refreshes the idle clock so the reaper leaves this session alone for
// another grace window. Called when a browser page that still shows this session
// as a tab sends a keepalive — a background tab has no socket of its own, so this
// is how an open page keeps the tabs it isn't currently viewing from being reaped.
func (s *session) markSeen() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// reapable reports whether the idle-session reaper should close this session: it
// has no attached client and has not been seen (last client left / last keepalive)
// for at least grace. A session with any client — a native terminal or the browser
// tab currently being viewed — is never reapable. Already-closed sessions report
// false so a concurrent reap and process-exit don't double-close.
func (s *session) reapable(grace time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.clients) > 0 {
		return false
	}
	return time.Since(s.lastSeen) >= grace
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
	// Everything here runs under s.mu, including queueing the client's opening
	// frames. This is what makes the replay correct: readLoop also holds s.mu when
	// it both records output into the buffer AND picks which clients to deliver it
	// to. Adding c to s.clients, snapshotting the buffer, and enqueuing the replay
	// as one locked step guarantees c's history snapshot and its live stream meet
	// exactly at the seam -- no bytes are dropped between them and none are sent
	// twice, and no live frame can reach c's channel ahead of the history.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c] = struct{}{}
	effCols, effRows := s.lastEffCols, s.lastEffRows

	// Start the client from a clean slate...
	c.queue(mustJSON(wsMessage{Type: "reset"}))
	// ...pin it to the authoritative grid BEFORE the replay, so the browser
	// reconstructs the history at the same width the native terminal used and its
	// line wrapping / scrollback come out identical (not reflowed to the phone).
	if effCols > 0 && effRows > 0 {
		c.queue(mustJSON(wsMessage{Type: "size", Cols: effCols, Rows: effRows}))
	}
	// Replay the retained scrollback to browser clients only. Unlike a naive
	// mid-stream replay, this is written into an xterm already sized to the ConPTY,
	// so the same cursor-relative frame updates land where they did originally and
	// only genuinely-scrolled lines remain as history -- a faithful copy of what the
	// desktop console shows. A native terminal (primary) keeps its own real console
	// scrollback and ignores this frame, so don't spend the gzip on it.
	if !c.primary {
		if frame := replayFrame(s.buffer.BytesFromLine()); frame != nil {
			c.queue(frame)
		}
	}
	c.queue(mustJSON(wsMessage{Type: "status", Message: "connected"}))
}

// replayFrame packages retained session output into a {type:"replay"} message:
// gzip-compressed (terminal text compresses heavily) then base64'd so it rides in
// the same JSON text channel as every other frame. Returns nil for an empty
// buffer so the caller can skip it. Built while s.mu is held; gzip of a few MiB is
// a few tens of ms, a one-time cost on the (infrequent) attach path.
func replayFrame(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return mustJSON(wsMessage{
		Type: "replay",
		Enc:  "gzip",
		Data: base64.StdEncoding.EncodeToString(buf.Bytes()),
	})
}

func (s *session) detach(c *client) {
	s.mu.Lock()
	delete(s.clients, c)
	// Start (or refresh) the idle clock: with this client gone the session may now
	// have none, and the reaper measures abandonment from here. Harmless when other
	// clients remain — reapable checks for those before ever consulting lastSeen.
	s.lastSeen = time.Now()
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

// minClientSizeLocked returns the cols/rows the shared ConPTY should hold, given
// that it can only be ONE size for every attached client. Authority is decided by
// FOCUS, so the terminal follows whichever screen the user is actually on:
//
//  1. If any browser is focused (active), the ConPTY tracks the focused
//     browser(s) — even over a native terminal. This is what lets the phone shrink
//     the grid to a readable width while you're using it; Claude Code reflows
//     cleanly on the width change (its one guaranteed-clean repaint case).
//  2. Otherwise a native terminal (primary) is authority: it CANNOT be resized by
//     moona, so when you're not looking at any browser the ConPTY snaps back to
//     the native terminal's size and it renders cleanly again.
//  3. Otherwise browsers drive the size (phone-only use, nothing to obey).
//
// Within a tier we take the smallest reported size so every client in that tier
// stays within the ConPTY. Clients with no reported size (cols/rows 0) are
// ignored. Caller holds s.mu.
func (s *session) minClientSizeLocked() (int, int) {
	if cols, rows := s.minActiveBrowserSizeLocked(); cols > 0 && rows > 0 {
		return cols, rows
	}
	cols, rows := s.minSizeLocked(true)
	if cols > 0 && rows > 0 {
		return cols, rows
	}
	return s.minSizeLocked(false)
}

// minActiveBrowserSizeLocked returns the smallest reported size across browser
// clients that are currently focused (active). Native (primary) clients never
// count — they don't report focus and can't be resized anyway. Returns 0,0 when
// no focused browser has reported a size. Caller holds s.mu.
func (s *session) minActiveBrowserSizeLocked() (int, int) {
	cols, rows := 0, 0
	for cl := range s.clients {
		if cl.primary || !cl.active {
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

// setActive records a browser client's focus state and, because focus can change
// which client is the size authority, recomputes the effective ConPTY size the
// same way resize() does — resizing the pseudoconsole and broadcasting the new
// size so every client conforms. Focusing the phone shrinks the ConPTY to fit it;
// backgrounding it hands authority back to the native terminal (or another
// focused browser).
func (s *session) setActive(c *client, active bool) error {
	s.mu.Lock()
	c.active = active
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
	for _, cl := range clients {
		cl.queue(msg)
	}
	return nil
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
		Title:   s.title,
		Cols:    cols,
		Rows:    rows,
		Clients: len(s.clients),
		Running: s.running,
	}
}

// scanTitleLocked feeds a chunk of raw ConPTY output through an incremental
// parser that watches for terminal window-title sequences — OSC 0 (icon+title)
// and OSC 2 (title) — of the form ESC ] 0 ; <text> BEL (or ...ST). The parser
// state lives on the session so a sequence split across two Reads still resolves.
// It returns true when a NEW title (different from the current one) was fully
// received, so the caller can trigger a tab-bar refresh. Caller holds s.mu.
//
// titleMax caps an accumulated title so a program that never terminates its OSC
// (or a hostile stream) can't grow the buffer without bound.
const titleMax = 512

func (s *session) scanTitleLocked(chunk []byte) bool {
	changed := false
	commit := func() {
		if s.tIsTitle && len(s.tBuf) > 0 {
			t := string(s.tBuf)
			if t != s.title {
				s.title = t
				changed = true
			}
		}
		s.tIsTitle = false
		s.tBuf = s.tBuf[:0]
	}
	for _, b := range chunk {
		switch s.tScan {
		case 0: // ground: hunt for ESC
			if b == 0x1b {
				s.tScan = 1
			}
		case 1: // after ESC: an OSC opens with ']'
			if b == ']' {
				s.tScan = 2
				s.tPs = s.tPs[:0]
			} else if b != 0x1b {
				s.tScan = 0
			}
		case 2: // reading the OSC command number (Ps) up to ';'
			if b >= '0' && b <= '9' {
				if len(s.tPs) < 4 {
					s.tPs = append(s.tPs, b)
				}
			} else if b == ';' {
				ps := string(s.tPs)
				s.tIsTitle = ps == "0" || ps == "2"
				s.tBuf = s.tBuf[:0]
				s.tScan = 3
			} else {
				s.tScan = 0 // not an OSC we track
			}
		case 3: // OSC payload: accumulate until BEL or ST (ESC \)
			if b == 0x07 { // BEL terminator
				commit()
				s.tScan = 0
			} else if b == 0x1b { // maybe the ESC of an ST terminator
				s.tScan = 4
			} else if s.tIsTitle && len(s.tBuf) < titleMax {
				s.tBuf = append(s.tBuf, b)
			}
		case 4: // saw ESC inside OSC payload: ST is ESC '\'
			if b == '\\' { // string terminator
				commit()
				s.tScan = 0
			} else {
				// Not a valid ST: the OSC ended abnormally. Commit what we have and
				// reprocess this byte from ground so a new ESC isn't lost.
				commit()
				s.tScan = 0
				if b == 0x1b {
					s.tScan = 1
				}
			}
		}
	}
	return changed
}

func (s *session) readLoop(pty *conpty.ConPty) {
	buf := make([]byte, 32*1024)
	for {
		n, err := pty.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.buffer.Write(chunk)
			titleChanged := s.scanTitleLocked(chunk)
			onTitle := s.onTitleChange
			clients := make([]*client, 0, len(s.clients))
			for c := range s.clients {
				clients = append(clients, c)
			}
			s.mu.Unlock()
			msg := mustJSON(wsMessage{Type: "output", Data: string(chunk)})
			for _, c := range clients {
				c.queue(msg)
			}
			// A new window title just means the tab bar's label changed; push the
			// refreshed session list so every browser relabels its tab live.
			if titleChanged && onTitle != nil {
				onTitle()
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
