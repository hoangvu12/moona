package main

import (
	_ "embed"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// indexHTML is the single-page terminal UI served at "/". It is kept in a
// standalone file (web/index.html) so the markup/JS can be edited with real
// HTML tooling; //go:embed inlines it into the binary at build time.
//
//go:embed web/index.html
var indexHTML string

// devMode reports whether MOONA_DEV is set to a non-empty, non-"0" value. In dev
// mode the terminal page is served fresh from disk on every request (instead of
// the //go:embed'd copy) and a watcher pushes a browser reload over the session
// WebSocket, so editing web/index.html shows up with just a refresh -- no
// `go build`, no daemon restart. Wired up by dev.ps1 / test-run.ps1.
func devMode() bool {
	v := os.Getenv("MOONA_DEV")
	return v != "" && v != "0"
}

// devWebPath is the on-disk location of index.html used in dev mode. It honors
// MOONA_DEV_WEB (prefer an absolute path -- the daemon may run from a different
// working dir than the repo root, e.g. an auto-spawned detached daemon) and
// otherwise falls back to the source-tree layout relative to the current dir.
func devWebPath() string {
	if p := os.Getenv("MOONA_DEV_WEB"); p != "" {
		return p
	}
	return filepath.Join("cmd", "moona", "web", "index.html")
}

var devWarnOnce sync.Once

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body := indexHTML
	if devMode() {
		if b, err := os.ReadFile(devWebPath()); err == nil {
			body = string(b)
		} else {
			// Fall back to the embedded copy so the page still loads; warn once
			// rather than on every request.
			devWarnOnce.Do(func() {
				log.Printf("dev: cannot read %s (%v); serving embedded page", devWebPath(), err)
			})
		}
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// seedDevSession starts one session on daemon boot (dev only) so a browser tab
// pointed at the sandbox reconnects to a live terminal after each rebuild-restart
// instead of the empty "no sessions" state. cmd is MOONA_DEV_SEED (e.g. a shell).
func (d *daemon) seedDevSession(cmd string) {
	cfg := config{
		host:        d.opts.host,
		port:        d.opts.port,
		token:       d.opts.token,
		commandLine: cmd,
		workDir:     mustGetwd(),
		cols:        100,
		rows:        30,
	}
	if _, err := d.hub.createSession(cfg); err != nil {
		log.Printf("dev: seed session (%s) failed: %v", cmd, err)
		return
	}
	log.Printf("dev: seeded session: %s", cmd)
}

// watchWebReload polls the dev index.html for changes and pushes a
// {type:"reload"} frame to every connected browser when it changes, so saving
// the file auto-refreshes the page. Dev-only; a lightweight mtime poll avoids
// adding an fsnotify dependency to an otherwise lean binary. It is a no-op (and
// logs once) if the file can't be found.
func (d *daemon) watchWebReload() {
	path := devWebPath()
	fi, err := os.Stat(path)
	if err != nil {
		log.Printf("dev: web live-reload disabled, cannot stat %s: %v", path, err)
		return
	}
	last := fi.ModTime()
	log.Printf("dev: watching %s for changes (browser auto-reload on)", path)
	reload := mustJSON(wsMessage{Type: "reload"})
	for {
		time.Sleep(300 * time.Millisecond)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if m := fi.ModTime(); m.After(last) {
			last = m
			d.hub.broadcast(reload)
		}
	}
}
