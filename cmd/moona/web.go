package main

import (
	_ "embed"
	"net/http"
)

// indexHTML is the single-page terminal UI served at "/". It is kept in a
// standalone file (web/index.html) so the markup/JS can be edited with real
// HTML tooling; //go:embed inlines it into the binary at build time.
//
//go:embed web/index.html
var indexHTML string

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(indexHTML))
}
