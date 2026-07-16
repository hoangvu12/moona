package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This file automates the Cloudflare named-tunnel setup so the user never has to
// run cloudflared by hand. Every step is idempotent and self-skipping: if the
// machine is already authorized (cert.pem present) the browser step is skipped;
// if a tunnel with our name already exists it is reused; an already-present DNS
// record is treated as success. So the first run does the full flow and later
// runs are near-instant.

// cloudflaredCertPath is where `cloudflared tunnel login` saves the account
// certificate. Its presence is how we detect an existing login.
func cloudflaredCertPath() string {
	if p := strings.TrimSpace(os.Getenv("TUNNEL_ORIGIN_CERT")); p != "" {
		return p
	}
	home, err := os.UserHomeDir() // %USERPROFILE% on Windows
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cloudflared", "cert.pem")
}

// cloudflaredLoggedIn reports whether this machine has already authorized with
// Cloudflare (so we can skip the interactive browser login).
func cloudflaredLoggedIn() bool {
	p := cloudflaredCertPath()
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

type cfTunnelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// cloudflaredListTunnels returns the account's existing tunnels (requires login).
func cloudflaredListTunnels(bin string) ([]cfTunnelInfo, error) {
	out, err := exec.Command(bin, "tunnel", "list", "--output", "json").Output()
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 || string(out) == "null" {
		return nil, nil
	}
	var tunnels []cfTunnelInfo
	if err := json.Unmarshal(out, &tunnels); err != nil {
		return nil, err
	}
	return tunnels, nil
}

// cloudflaredFindTunnel returns the UUID of an existing tunnel with the given
// name (or UUID — config may store either), or "" if none exists.
func cloudflaredFindTunnel(bin, nameOrID string) (string, error) {
	tunnels, err := cloudflaredListTunnels(bin)
	if err != nil {
		return "", err
	}
	for _, t := range tunnels {
		if strings.EqualFold(t.Name, nameOrID) || strings.EqualFold(t.ID, nameOrID) {
			return t.ID, nil
		}
	}
	return "", nil
}

// cloudflaredCredsPath is where cloudflared expects a named tunnel's credentials
// file: <tunnel-id>.json in the same directory as the account certificate.
func cloudflaredCredsPath(id string) string {
	certPath := cloudflaredCertPath()
	if certPath == "" || id == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(certPath), id+".json")
}

// ensureCloudflareCreds makes sure the tunnel's local credentials file exists,
// regenerating it from the Cloudflare account when missing. Only `cloudflared
// tunnel create` writes that file, so a tunnel created on another machine — or a
// wiped ~/.cloudflared — leaves `tunnel run` failing forever with "tunnel
// credentials file not found". With the account cert present, the file can be
// minted again via `cloudflared tunnel token --cred-file`.
func ensureCloudflareCreds(bin, tunnel string) error {
	if !cloudflaredLoggedIn() {
		return errors.New("Cloudflare isn't authorized on this machine (no cert.pem) — run `moona setup` to re-authorize")
	}
	id, err := cloudflaredFindTunnel(bin, tunnel)
	if err != nil || id == "" {
		// Listing can fail transiently (network); a missing tunnel is a config
		// problem `tunnel run` reports clearly itself. Neither is the stranded-
		// credentials case this fixes, so let the run proceed and speak for itself.
		return nil
	}
	credsPath := cloudflaredCredsPath(id)
	if credsPath == "" {
		return nil
	}
	if fi, err := os.Stat(credsPath); err == nil && !fi.IsDir() {
		return nil
	}
	terminalPrintln(os.Stderr, "tunnel credentials file missing; regenerating from your Cloudflare account...")
	if out, err := exec.Command(bin, "tunnel", "token", "--cred-file", credsPath, id).CombinedOutput(); err != nil {
		return fmt.Errorf("regenerate tunnel credentials: %s — run `moona setup` to re-authorize Cloudflare", firstLine(out))
	}
	return nil
}

// provisionCloudflare makes a named tunnel ready end-to-end: authorize (browser,
// only if needed), create the tunnel if missing, and point the hostname at it. It
// prints progress to the normal terminal, so call it AFTER the wizard's alt-screen
// TUI has exited. cfg.CFTunnel is defaulted/persisted by the caller.
func provisionCloudflare(cfg *userConfig) error {
	host := normalizeHost(cfg.CFHostname)
	if host == "" {
		return errors.New("no hostname was provided")
	}
	cfg.CFHostname = host
	if strings.TrimSpace(cfg.CFTunnel) == "" {
		cfg.CFTunnel = "moona"
	}

	bin, err := ensureCloudflared()
	if err != nil {
		return err
	}

	// 1. Authorize (one browser click) — skipped if a cert already exists.
	if cloudflaredLoggedIn() {
		fmt.Println("• Cloudflare already authorized on this machine — skipping login.")
	} else {
		fmt.Println("• Opening your browser to authorize Cloudflare. Sign in and pick your domain...")
		login := exec.Command(bin, "tunnel", "login")
		login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := login.Run(); err != nil {
			return fmt.Errorf("authorize step failed: %w", err)
		}
		if !cloudflaredLoggedIn() {
			return errors.New("authorization did not complete (no certificate was saved)")
		}
	}

	// 2. Reuse or create the tunnel.
	id, err := cloudflaredFindTunnel(bin, cfg.CFTunnel)
	if err != nil {
		return fmt.Errorf("could not list tunnels (is your domain on this Cloudflare account?): %w", err)
	}
	if id == "" {
		fmt.Printf("• Creating tunnel %q...\n", cfg.CFTunnel)
		if out, err := exec.Command(bin, "tunnel", "create", cfg.CFTunnel).CombinedOutput(); err != nil {
			return fmt.Errorf("create tunnel: %s", firstLine(out))
		}
	} else {
		fmt.Printf("• Reusing existing tunnel %q.\n", cfg.CFTunnel)
		// A tunnel created on another machine (or a wiped ~/.cloudflared) has no
		// local credentials file, which `tunnel run` needs; regenerate it now.
		if err := ensureCloudflareCreds(bin, cfg.CFTunnel); err != nil {
			return err
		}
	}

	// 3. Point the hostname at the tunnel (idempotent: an existing record is fine).
	fmt.Printf("• Routing https://%s to the tunnel...\n", host)
	if out, err := exec.Command(bin, "tunnel", "route", "dns", cfg.CFTunnel, host).CombinedOutput(); err != nil {
		low := strings.ToLower(string(out))
		if !strings.Contains(low, "already exists") && !strings.Contains(low, "already configured") {
			return fmt.Errorf("route dns: %s", firstLine(out))
		}
		fmt.Println("  (a DNS record for that host already existed — reusing it)")
	}

	fmt.Printf("✓ Cloudflare tunnel ready — your permanent phone link is https://%s\n", host)
	return nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
