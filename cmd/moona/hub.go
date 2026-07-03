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
	h.sessions[id] = sess
	h.mu.Unlock()

	if err := sess.ensureStarted(); err != nil {
		// Roll back the registration if the process never launched.
		h.remove(id)
		return nil, err
	}
	return sess, nil
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
