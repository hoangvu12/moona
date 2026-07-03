package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func testModel() uiModel {
	m := newUIModel(daemonState{LocalURL: "http://127.0.0.1:8799"}, "moona.exe")
	m.width, m.height = 100, 30
	m.sessions = []sessionInfo{
		{ID: "1", Command: "cmd.exe", Cols: 90, Rows: 25, Clients: 0, Running: true},
		{ID: "2", Command: "pwsh.exe", Cols: 100, Rows: 30, Clients: 1, Running: true},
	}
	return m
}

func key(s string) tea.KeyMsg {
	switch s {
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func send(m uiModel, msg tea.Msg) uiModel {
	next, _ := m.Update(msg)
	return next.(uiModel)
}

func TestCursorMovement(t *testing.T) {
	m := testModel()
	if m.cursor != 0 {
		t.Fatalf("cursor should start at 0, got %d", m.cursor)
	}
	m = send(m, key("down"))
	if m.cursor != 1 {
		t.Fatalf("down -> cursor 1, got %d", m.cursor)
	}
	m = send(m, key("down")) // clamp at last
	if m.cursor != 1 {
		t.Fatalf("down at end stays 1, got %d", m.cursor)
	}
	m = send(m, key("up"))
	if m.cursor != 0 {
		t.Fatalf("up -> cursor 0, got %d", m.cursor)
	}
	if s := m.selected(); s == nil || s.ID != "1" {
		t.Fatalf("selected should be session 1")
	}
}

func TestNewSessionInput(t *testing.T) {
	m := testModel()
	m = send(m, key("n"))
	if m.mode != modeNew {
		t.Fatalf("n should enter modeNew")
	}
	for _, r := range []string{"c", "l", "a", "u", "d", "e"} {
		m = send(m, key(r))
	}
	if m.input != "claude" {
		t.Fatalf("input should be 'claude', got %q", m.input)
	}
	m = send(m, key("backspace"))
	if m.input != "claud" {
		t.Fatalf("backspace -> 'claud', got %q", m.input)
	}
	m = send(m, key("esc"))
	if m.mode != modeList || m.input != "" {
		t.Fatalf("esc should cancel back to list and clear input")
	}
}

func TestKillConfirm(t *testing.T) {
	m := testModel()
	m = send(m, key("down")) // select session 2
	m = send(m, key("x"))
	if m.mode != modeConfirmKill {
		t.Fatalf("x should enter modeConfirmKill")
	}
	// A non-confirming key cancels.
	m = send(m, key("n"))
	if m.mode != modeList {
		t.Fatalf("non-y key should cancel kill")
	}
}

func TestOverlaysDismiss(t *testing.T) {
	m := testModel()
	m = send(m, key("c"))
	if m.mode != modeQR {
		t.Fatalf("c should open QR overlay")
	}
	m = send(m, key("x")) // any key dismisses
	if m.mode != modeList {
		t.Fatalf("any key should dismiss QR overlay")
	}
	m = send(m, key("?"))
	if m.mode != modeHelp {
		t.Fatalf("? should open help overlay")
	}
}

func TestSessionsMsgClampsCursor(t *testing.T) {
	m := testModel()
	m = send(m, key("down")) // cursor 1
	// A refresh that drops to one session must clamp the cursor.
	m = send(m, sessionsMsg{sessions: []sessionInfo{{ID: "1", Running: true}}})
	if m.cursor != 0 {
		t.Fatalf("cursor should clamp to 0 after list shrinks, got %d", m.cursor)
	}
}

func TestViewRendersWithoutPanic(t *testing.T) {
	m := testModel()
	for _, mode := range []uiMode{modeList, modeNew, modeConfirmKill, modeQR, modeHelp} {
		m.mode = mode
		if out := m.View(); out == "" {
			t.Fatalf("View() empty for mode %d", mode)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 4); got != "abc…" {
		t.Fatalf("truncate long: got %q", got)
	}
	if got := truncate("ab", 4); got != "ab" {
		t.Fatalf("truncate short: got %q", got)
	}
}

func TestIndexByteDetach(t *testing.T) {
	if i := indexByte([]byte{'a', 'b', detachByte, 'c'}, detachByte); i != 2 {
		t.Fatalf("indexByte should find detach at 2, got %d", i)
	}
	if i := indexByte([]byte("abc"), detachByte); i != -1 {
		t.Fatalf("indexByte should return -1 when absent, got %d", i)
	}
}
