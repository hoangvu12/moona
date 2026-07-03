package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"testing"
)

// drain collects every queued frame from a client's send channel without blocking.
func drain(c *client) [][]byte {
	var out [][]byte
	for {
		select {
		case m := <-c.send:
			out = append(out, m)
		default:
			return out
		}
	}
}

// decodeReplay reverses replayFrame exactly as the browser does: base64 -> gunzip.
func decodeReplay(t *testing.T, frame []byte) []byte {
	t.Helper()
	var msg wsMessage
	if err := json.Unmarshal(frame, &msg); err != nil {
		t.Fatalf("replay frame is not valid JSON: %v", err)
	}
	if msg.Type != "replay" || msg.Enc != "gzip" {
		t.Fatalf("unexpected replay frame: type=%q enc=%q", msg.Type, msg.Enc)
	}
	gz, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		t.Fatalf("replay data is not valid base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("replay data is not valid gzip: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip failed: %v", err)
	}
	return raw
}

// findFrame returns the first frame in msgs whose JSON "type" equals typ.
func findFrame(msgs [][]byte, typ string) []byte {
	for _, m := range msgs {
		var msg wsMessage
		if json.Unmarshal(m, &msg) == nil && msg.Type == typ {
			return m
		}
	}
	return nil
}

func TestAttachReplaysHistory(t *testing.T) {
	s := newSession(config{})
	s.lastEffCols, s.lastEffRows = 80, 24
	// Simulate captured ConPTY output: a fragment first line (the ring drops from
	// the front mid-line), then several whole lines of scrollable history.
	s.buffer.Write([]byte("gment of an old line\r\nline one\r\nline two\r\nline three\r\n"))

	c := &client{send: make(chan []byte, 64)}
	s.attach(c)
	msgs := drain(c)

	// Opening frames must be present and the size must precede the replay so the
	// browser sizes the grid before reconstructing history.
	sizeIdx, replayIdx := -1, -1
	for i, m := range msgs {
		var msg wsMessage
		_ = json.Unmarshal(m, &msg)
		switch msg.Type {
		case "size":
			sizeIdx = i
		case "replay":
			replayIdx = i
		}
	}
	if findFrame(msgs, "reset") == nil {
		t.Fatal("attach did not send a reset frame")
	}
	if sizeIdx < 0 || replayIdx < 0 {
		t.Fatalf("missing frames: sizeIdx=%d replayIdx=%d", sizeIdx, replayIdx)
	}
	if sizeIdx > replayIdx {
		t.Fatalf("size frame (%d) must come before replay frame (%d)", sizeIdx, replayIdx)
	}

	got := decodeReplay(t, msgs[replayIdx])
	want := "line one\r\nline two\r\nline three\r\n"
	if string(got) != want {
		t.Fatalf("replay history mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestAttachEmptyBufferSkipsReplay(t *testing.T) {
	s := newSession(config{})
	c := &client{send: make(chan []byte, 64)}
	s.attach(c)
	if findFrame(drain(c), "replay") != nil {
		t.Fatal("empty session should not send a replay frame")
	}
}
