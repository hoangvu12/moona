package main

import (
	"bytes"
	"sync"
)

type ringBuffer struct {
	mu   sync.Mutex
	data []byte
	cap  int
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{cap: capacity}
}

func (r *ringBuffer) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= r.cap {
		r.data = append(r.data[:0], p[len(p)-r.cap:]...)
		return
	}
	r.data = append(r.data, p...)
	if overflow := len(r.data) - r.cap; overflow > 0 {
		copy(r.data, r.data[overflow:])
		r.data = r.data[:r.cap]
	}
}

func (r *ringBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.data...)
}

// BytesFromLine returns a copy of the buffered bytes starting just after the
// first newline. The ring drops from the front at an arbitrary byte boundary, so
// its first line is usually a fragment; skipping to the first real line start
// keeps a replayed snapshot from beginning mid-line (which would garble the top
// row of reconstructed scrollback). Returns everything if there is no newline yet.
func (r *ringBuffer) BytesFromLine() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	data := r.data
	if i := bytes.IndexByte(data, '\n'); i >= 0 && i+1 <= len(data) {
		data = data[i+1:]
	}
	return append([]byte(nil), data...)
}

func (r *ringBuffer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = r.data[:0]
}
