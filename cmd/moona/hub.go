package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// sessionInfo is the JSON shape returned by the control API and consumed by the
// browser tab bar and `moona ls`.
type sessionInfo struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	// Title is the live terminal window title (OSC 0/2), shown as the browser tab
	// label. Empty until the program sets one; the browser then falls back to Command.
	Title   string `json:"title,omitempty"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
	Clients int    `json:"clients"`
	Running bool   `json:"running"`
}

// hub is the daemon's registry of live terminal sessions. Each session owns its
// own ConPTY; the hub just tracks them by id, mints ids, and knows when the last
// session went away (for idle-exit).
type hub struct {
	mu        sync.Mutex
	sessions  map[string]*session
	seq       int
	lastEmpty time.Time // when the session count last dropped to zero
	// onChange, if set, is invoked (off-lock) whenever the session set changes, so
	// the daemon can push the fresh list to every connected browser instead of
	// making them poll. Must not be called while holding h.mu.
	onChange func()
	// Title-change throttle. Window titles can update rapidly (a program showing
	// progress in its title), and each push traverses the tunnel — which we keep
	// deliberately quiet to stay under metered-tunnel request caps. Coalesce title
	// pushes to at most one per second, with a trailing push for the last change.
	titleMu      sync.Mutex
	titleTimer   *time.Timer
	titlePending bool
}

func newHub() *hub {
	return &hub{
		sessions:  make(map[string]*session),
		lastEmpty: time.Now(),
	}
}

// createSession registers a new session, wires its exit callback, and starts its
// ConPTY. On start failure nothing is registered.
func (h *hub) createSession(cfg config) (*session, error) {
	sess := newSession(cfg)

	h.mu.Lock()
	h.seq++
	id := fmt.Sprintf("%d", h.seq)
	sess.id = id
	sess.onExit = func() { h.remove(id) }
	sess.onTitleChange = h.notifyTitleChange
	h.sessions[id] = sess
	h.mu.Unlock()

	if err := sess.ensureStarted(); err != nil {
		// Roll back the registration if the process never launched.
		h.remove(id)
		return nil, err
	}
	h.notifyChange()
	return sess, nil
}

// notifyChange invokes the onChange hook off-lock. Safe to call with h.mu not held.
func (h *hub) notifyChange() {
	h.mu.Lock()
	cb := h.onChange
	h.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// notifyTitleChange pushes an updated session list on a title change, throttled
// to at most once per second (leading edge fires immediately; a trailing edge
// fires once more if further changes arrived during the window). This keeps a
// chatty title from flooding the tunnel while still reflecting the latest label.
func (h *hub) notifyTitleChange() {
	h.titleMu.Lock()
	if h.titleTimer != nil {
		// A push went out within the last second; remember to send one more.
		h.titlePending = true
		h.titleMu.Unlock()
		return
	}
	h.titleTimer = time.AfterFunc(time.Second, func() {
		h.titleMu.Lock()
		pending := h.titlePending
		h.titlePending = false
		h.titleTimer = nil
		h.titleMu.Unlock()
		if pending {
			h.notifyTitleChange()
		}
	})
	h.titleMu.Unlock()
	h.notifyChange()
}

func (h *hub) get(id string) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[id]
}

func (h *hub) remove(id string) {
	h.mu.Lock()
	sess, ok := h.sessions[id]
	if ok {
		delete(h.sessions, id)
		if len(h.sessions) == 0 {
			h.lastEmpty = time.Now()
		}
	}
	h.mu.Unlock()
	if ok {
		sess.close()
		h.notifyChange()
	}
}

// touch refreshes the idle clock of every named session that still exists, so an
// open browser page keeps the tabs it is showing (including the ones it is not
// currently viewing, which have no socket) from being reaped. Unknown ids — tabs
// for sessions that already ended — are ignored.
func (h *hub) touch(ids []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		if s := h.sessions[id]; s != nil {
			s.markSeen()
		}
	}
}

// reapIdle closes every session that has had no client and no keepalive for at
// least grace, dropping them from the registry, and returns their ids for logging.
// This is what makes a tab with no active client disappear on its own instead of
// lingering forever. It mirrors remove()'s bookkeeping (lastEmpty + onChange) so a
// reap that empties the daemon still arms idle-exit and refreshes browser tab bars.
func (h *hub) reapIdle(grace time.Duration) []string {
	h.mu.Lock()
	var doomed []*session
	var ids []string
	for id, s := range h.sessions {
		if s.reapable(grace) {
			doomed = append(doomed, s)
			ids = append(ids, id)
			delete(h.sessions, id)
		}
	}
	if len(doomed) > 0 && len(h.sessions) == 0 {
		h.lastEmpty = time.Now()
	}
	h.mu.Unlock()
	for _, s := range doomed {
		s.close()
	}
	if len(doomed) > 0 {
		h.notifyChange()
	}
	return ids
}

// broadcast queues msg to every client across every session. Used to push the
// session list to all connected browsers on any change.
func (h *hub) broadcast(msg []byte) {
	h.mu.Lock()
	sessions := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()
	for _, s := range sessions {
		s.queueToAll(msg)
	}
}

// list returns a snapshot of every session, ordered by numeric id so the tab bar
// and `moona ls` are stable across refreshes.
func (h *hub) list() []sessionInfo {
	h.mu.Lock()
	sessions := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()

	infos := make([]sessionInfo, 0, len(sessions))
	for _, s := range sessions {
		infos = append(infos, s.snapshot())
	}
	sort.Slice(infos, func(i, j int) bool {
		return sessionIDLess(infos[i].ID, infos[j].ID)
	})
	return infos
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

// idleFor reports whether there have been zero sessions for at least d. Used by
// auto-started daemons to shut themselves down when nothing is using them.
func (h *hub) idleFor(d time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions) == 0 && time.Since(h.lastEmpty) >= d
}

func (h *hub) closeAll() {
	h.mu.Lock()
	sessions := make([]*session, 0, len(h.sessions))
	for id, s := range h.sessions {
		sessions = append(sessions, s)
		delete(h.sessions, id)
	}
	h.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
}

func sessionIDLess(a, b string) bool {
	an, aok := atoiSafe(a)
	bn, bok := atoiSafe(b)
	if aok && bok {
		return an < bn
	}
	return a < b
}

func atoiSafe(s string) (int, bool) {
	n := 0
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}
