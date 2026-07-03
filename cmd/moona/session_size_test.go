package main

import "testing"

// Tests the focus-follows size-authority logic in minClientSizeLocked: a focused
// (active) browser outranks a native primary terminal, and authority reverts when
// it backgrounds. One shared ConPTY, one size — this decides whose size that is.

func newTestSession() *session {
	return &session{clients: make(map[*client]struct{})}
}

func (s *session) addClient(cols, rows int, primary, active bool) *client {
	c := &client{cols: cols, rows: rows, primary: primary, active: active}
	s.clients[c] = struct{}{}
	return c
}

func assertSize(t *testing.T, s *session, wantC, wantR int, msg string) {
	t.Helper()
	c, r := s.minClientSizeLocked()
	if c != wantC || r != wantR {
		t.Fatalf("%s: got %dx%d, want %dx%d", msg, c, r, wantC, wantR)
	}
}

func TestSizeAuthority(t *testing.T) {
	// A native terminal alone is authority (unchanged legacy behavior).
	s := newTestSession()
	s.addClient(107, 32, true, false)
	assertSize(t, s, 107, 32, "primary only")

	// A backgrounded browser does NOT steal authority from the native terminal.
	phone := s.addClient(46, 31, false, false)
	assertSize(t, s, 107, 32, "primary + inactive browser")

	// The user looks at the phone: the focused browser outranks the primary, so
	// the ConPTY shrinks to fit the phone.
	phone.active = true
	assertSize(t, s, 46, 31, "primary + active browser -> phone wins")

	// Backgrounding the phone hands authority back to the native terminal.
	phone.active = false
	assertSize(t, s, 107, 32, "phone backgrounded -> native reclaims")
}

func TestSizeAuthority_MultipleActiveBrowsersTakeSmallest(t *testing.T) {
	s := newTestSession()
	s.addClient(107, 32, true, false)
	s.addClient(80, 40, false, true)
	s.addClient(46, 31, false, true)
	// Two focused browsers at once: smallest in the tier keeps both clean.
	assertSize(t, s, 46, 31, "two active browsers -> smallest")
}

func TestSizeAuthority_NoPrimaryBrowsersDrive(t *testing.T) {
	// Phone-only use: no native terminal, so browsers drive the size even when
	// none has reported focus yet (backward compatible with old clients).
	s := newTestSession()
	s.addClient(50, 30, false, false)
	s.addClient(60, 40, false, false)
	assertSize(t, s, 50, 30, "no primary, no active -> min over all")
}

func TestSizeAuthority_ActiveWithoutReportedSizeIgnored(t *testing.T) {
	// An active browser that hasn't measured itself yet (cols/rows 0) must not
	// collapse the ConPTY to 0; authority falls through to the native terminal.
	s := newTestSession()
	s.addClient(107, 32, true, false)
	s.addClient(0, 0, false, true)
	assertSize(t, s, 107, 32, "active but unsized browser -> ignored")
}
