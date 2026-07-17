package main

import (
	"testing"
	"time"
)

// Tests the idle-session reaper: a tab with no active client (and no browser
// keepalive) is closed after the grace window, while an attached or freshly-seen
// session is left alone. This is what stops detached/abandoned terminals from
// lingering in the daemon forever.

const testGrace = 2 * time.Minute

func TestReapable_ClientAttachedNeverReaped(t *testing.T) {
	s := newTestSession()
	s.lastSeen = time.Now().Add(-time.Hour) // long idle...
	s.addClient(80, 24, true, false)        // ...but a native terminal is attached.
	if s.reapable(testGrace) {
		t.Fatal("session with an attached client must never be reapable")
	}
}

func TestReapable_RecentlySeenSurvives(t *testing.T) {
	s := newTestSession()
	s.lastSeen = time.Now() // last client just left / just got a keepalive
	if s.reapable(testGrace) {
		t.Fatal("session seen within the grace window must not be reapable")
	}
}

func TestReapable_IdlePastGraceIsReaped(t *testing.T) {
	s := newTestSession()
	s.lastSeen = time.Now().Add(-testGrace - time.Second)
	if !s.reapable(testGrace) {
		t.Fatal("clientless session idle past the grace window must be reapable")
	}
}

func TestReapable_ClosedIsNotReaped(t *testing.T) {
	s := newTestSession()
	s.lastSeen = time.Now().Add(-time.Hour)
	s.closed = true
	if s.reapable(testGrace) {
		t.Fatal("an already-closed session must not be reaped again")
	}
}

func TestMarkSeenResetsIdleClock(t *testing.T) {
	s := newTestSession()
	s.lastSeen = time.Now().Add(-time.Hour)
	if !s.reapable(testGrace) {
		t.Fatal("precondition: session should be reapable before markSeen")
	}
	s.markSeen()
	if s.reapable(testGrace) {
		t.Fatal("markSeen must refresh the idle clock so the session survives")
	}
}

func TestHubReapIdle_RemovesOnlyAbandonedSessions(t *testing.T) {
	h := newHub()

	// Idle past the grace, no client -> should be reaped.
	idle := newTestSession()
	idle.id = "idle"
	idle.lastSeen = time.Now().Add(-testGrace - time.Second)
	h.sessions["idle"] = idle

	// Idle past the grace but still has a client -> must survive.
	busy := newTestSession()
	busy.id = "busy"
	busy.lastSeen = time.Now().Add(-time.Hour)
	busy.addClient(80, 24, false, true)
	h.sessions["busy"] = busy

	// Clientless but seen recently (open browser keepalive) -> must survive.
	fresh := newTestSession()
	fresh.id = "fresh"
	fresh.lastSeen = time.Now()
	h.sessions["fresh"] = fresh

	reaped := h.reapIdle(testGrace)

	if len(reaped) != 1 || reaped[0] != "idle" {
		t.Fatalf("expected only [idle] reaped, got %v", reaped)
	}
	if h.get("idle") != nil {
		t.Fatal("reaped session should be gone from the registry")
	}
	if h.get("busy") == nil || h.get("fresh") == nil {
		t.Fatal("a session with a client or a recent keepalive must survive the reaper")
	}
}

func TestHubTouch_KeepsListedSessionsAlive(t *testing.T) {
	h := newHub()
	s := newTestSession()
	s.id = "1"
	s.lastSeen = time.Now().Add(-time.Hour)
	h.sessions["1"] = s

	// A keepalive naming this session (and an unknown, already-ended one) refreshes
	// its clock; the unknown id is simply ignored.
	h.touch([]string{"1", "gone"})

	if s.reapable(testGrace) {
		t.Fatal("touch should have refreshed the session's idle clock")
	}
}
