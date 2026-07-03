package main

import "sync"

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

func (r *ringBuffer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = r.data[:0]
}
