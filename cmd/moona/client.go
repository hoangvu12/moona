package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// apiClient talks to a running daemon's control API using its recorded URL and
// token.
type apiClient struct {
	base  string
	token string
}

func newAPIClient(st daemonState) *apiClient {
	return &apiClient{base: strings.TrimRight(st.LocalURL, "/"), token: st.Token}
}

func (c *apiClient) do(method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("X-Moona-Token", c.token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return client.Do(req)
}

func (c *apiClient) createSession(cmd, cwd string, cols, rows int) (string, error) {
	resp, err := c.do(http.MethodPost, "/api/sessions", map[string]any{
		"cmd": cmd, "cwd": cwd, "cols": cols, "rows": rows,
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError("create session", resp)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (c *apiClient) listSessions() ([]sessionInfo, error) {
	resp, err := c.do(http.MethodGet, "/api/sessions", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError("list sessions", resp)
	}
	var out []sessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *apiClient) deleteSession(id string) error {
	resp, err := c.do(http.MethodDelete, "/api/sessions/"+id, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return apiError("kill session", resp)
	}
	return nil
}

func (c *apiClient) shutdown() error {
	resp, err := c.do(http.MethodPost, "/api/shutdown", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return apiError("shutdown", resp)
	}
	return nil
}

func apiError(what string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("%s: %s", what, msg)
}

// ----- CLI commands that talk to the daemon -----

func runLs(_ []string) error {
	st, ok := daemonAlive()
	if !ok {
		fmt.Println("moona daemon is not running. Start a session with, e.g., `moona claude`.")
		return nil
	}
	sessions, err := newAPIClient(st).listSessions()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("No active sessions. Start one with, e.g., `moona claude`.")
		return nil
	}
	fmt.Printf("%-4s  %-6s  %-9s  %s\n", "ID", "CLIENTS", "SIZE", "COMMAND")
	for _, s := range sessions {
		fmt.Printf("%-4s  %-6d  %-9s  %s\n", s.ID, s.Clients, fmt.Sprintf("%dx%d", s.Cols, s.Rows), s.Command)
	}
	return nil
}

func runKill(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: moona kill <session-id>  (see `moona ls`)")
	}
	st, ok := daemonAlive()
	if !ok {
		return fmt.Errorf("moona daemon is not running")
	}
	if err := newAPIClient(st).deleteSession(args[0]); err != nil {
		return err
	}
	fmt.Println("killed session", args[0])
	return nil
}

func runURL(_ []string) error {
	st, ok := daemonAlive()
	if !ok {
		return fmt.Errorf("moona daemon is not running")
	}
	fmt.Println("Local: ", browserURL(st.LocalURL, st.Token))
	if st.PublicURL != "" {
		fmt.Println("Public:", browserURL(st.PublicURL, st.Token))
	} else {
		fmt.Println("Public: (no tunnel)")
	}
	return nil
}

func runQR(_ []string) error {
	st, ok := daemonAlive()
	if !ok {
		return fmt.Errorf("moona daemon is not running")
	}
	printDashboard(st.LocalURL, st.PublicURL, st.Host, st.Token, true)
	return nil
}

func stopDaemon() error {
	st, ok := daemonAlive()
	if !ok {
		fmt.Println("moona daemon is not running.")
		clearState()
		return nil
	}
	if err := newAPIClient(st).shutdown(); err != nil {
		return err
	}
	// Wait until it is actually down so a following `moona status`/restart sees a
	// clean slate rather than the still-terminating daemon.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := daemonAlive(); !ok {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("moona daemon stopped.")
	return nil
}

func printDaemonStatus() error {
	st, ok := daemonAlive()
	if !ok {
		fmt.Println("moona daemon: not running")
		return nil
	}
	fmt.Println("moona daemon: running")
	fmt.Println("  pid:   ", st.PID)
	fmt.Println("  local: ", st.LocalURL)
	if st.PublicURL != "" {
		fmt.Println("  public:", st.PublicURL)
	}
	sessions, err := newAPIClient(st).listSessions()
	if err == nil {
		fmt.Println("  sessions:", len(sessions))
	}
	return nil
}

func nowStamp() string {
	return time.Now().Format(time.RFC3339)
}
