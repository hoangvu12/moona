package main

import (
	"regexp"
	"testing"
)

func TestFindTunnelURLNgrok(t *testing.T) {
	re := regexp.MustCompile(`https?://[^\s\]"'<>]+`)
	cases := map[string]string{
		`t=2026 lvl=info msg="started tunnel" url=https://moona-abc.ngrok-free.app`: "https://moona-abc.ngrok-free.app",
		`url=https://foo.ngrok.app`:                      "https://foo.ngrok.app",
		`https://bar.ngrok.io/`:                          "https://bar.ngrok.io/",
		`no url on this line`:                            "",
		`visit https://dashboard.ngrok.com to configure`: "", // dashboard host must not match
	}
	for line, want := range cases {
		if got := findTunnelURL(line, re); got != want {
			t.Errorf("findTunnelURL(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestFindTunnelURLStillMatchesLegacy(t *testing.T) {
	re := regexp.MustCompile(`https?://[^\s\]"'<>]+`)
	if got := findTunnelURL("https://happy-cat-123.trycloudflare.com", re); got != "https://happy-cat-123.trycloudflare.com" {
		t.Errorf("trycloudflare host should still match, got %q", got)
	}
}

func TestLineHasReadyMarker(t *testing.T) {
	markers := []string{"registered tunnel connection"}
	if !lineHasReadyMarker("INF Registered tunnel connection connIndex=0", markers) {
		t.Error("expected ready marker to match case-insensitively")
	}
	if lineHasReadyMarker("just starting up", markers) {
		t.Error("unrelated line should not match")
	}
	if lineHasReadyMarker("anything", nil) {
		t.Error("no markers should never match")
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"https://moona.example.com/": "moona.example.com",
		"http://foo.ngrok-free.app":  "foo.ngrok-free.app",
		"  bar.ts.net  ":             "bar.ts.net",
		"":                           "",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProviderIsPublic(t *testing.T) {
	for _, p := range []string{providerQuick, providerNgrok, providerCFNamed, ""} {
		if !providerIsPublic(p) {
			t.Errorf("provider %q should be public", p)
		}
	}
	if providerIsPublic(providerLocal) {
		t.Error("local provider should not be public")
	}
}

func TestTunnelEnabledFor(t *testing.T) {
	if tunnelEnabledFor(providerLocal) {
		t.Error("local provider should not enable a tunnel")
	}
	for _, p := range []string{providerQuick, providerNgrok, providerCFNamed, ""} {
		if !tunnelEnabledFor(p) {
			t.Errorf("provider %q should enable a tunnel", p)
		}
	}
}

func TestGenerateTokenIsRandomHex(t *testing.T) {
	a, b := generateToken(), generateToken()
	if a == "" || b == "" {
		t.Fatal("token should be non-empty")
	}
	if a == b {
		t.Error("two generated tokens should differ")
	}
	if len(a) != 32 {
		t.Errorf("expected 32 hex chars, got %d (%q)", len(a), a)
	}
}

func TestMaskSecret(t *testing.T) {
	if got := maskSecret("2abc12345678def9"); got != "2ab••••••••••ef9" {
		t.Errorf("maskSecret long = %q", got)
	}
	if got := maskSecret("short"); got != "•••••" {
		t.Errorf("maskSecret short = %q", got)
	}
}
