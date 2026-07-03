package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// Tunnel provider ids stored in userConfig.TunnelProvider. These decide how the
// daemon exposes the phone web page.
const (
	providerQuick   = "quick"            // Cloudflare Quick Tunnel (ephemeral URL) — the zero-setup default
	providerNgrok   = "ngrok"            // ngrok reserved domain (stable URL, no domain needed)
	providerCFNamed = "cloudflare-named" // Cloudflare named tunnel on your own hostname (stable URL)
	providerLocal   = "local"            // local network only, no public tunnel
)

// userConfig is the persistent, user-chosen configuration written by the
// onboarding wizard to %LOCALAPPDATA%\moona\config.json. It is separate from the
// ephemeral daemonState (daemon.json), which only records a running daemon's
// discovery info. Absence of this file means "not onboarded yet" and the daemon
// falls back to the historical Quick Tunnel behavior, so existing installs are
// unaffected.
type userConfig struct {
	Onboarded bool `json:"onboarded"`
	// TunnelProvider is one of the provider* constants. Empty is treated as
	// providerQuick for backward compatibility.
	TunnelProvider string `json:"tunnelProvider,omitempty"`
	// NgrokAuthtoken/NgrokDomain configure the ngrok provider. The domain is a
	// reserved static domain the user created at dashboard.ngrok.com so the URL is
	// stable across restarts.
	NgrokAuthtoken string `json:"ngrokAuthtoken,omitempty"`
	NgrokDomain    string `json:"ngrokDomain,omitempty"`
	// CFHostname/CFTunnel configure the Cloudflare named-tunnel provider.
	// CFHostname is the public https host (e.g. moona.example.com) the user routed
	// to their tunnel; CFTunnel is the tunnel name/UUID cloudflared runs.
	CFHostname string `json:"cfHostname,omitempty"`
	CFTunnel   string `json:"cfTunnel,omitempty"`
	// Token, when set, is an app token every client must present. The wizard
	// auto-generates one for public providers so the out-of-box state is never an
	// unauthenticated public shell.
	Token string `json:"token,omitempty"`
	// PhoneConnectedOnce becomes true the first time a browser connects. Until then
	// `moona claude` shows the connect-phone panel (QR + link) on session start; after
	// that it starts instantly. The daemon sets this.
	PhoneConnectedOnce bool `json:"phoneConnectedOnce,omitempty"`
}

func configPath() string { return filepath.Join(stateDir(), "config.json") }

// readUserConfig loads config.json. A missing file returns a zero userConfig with
// ok=false so callers can distinguish "never onboarded" from "onboarded with
// defaults".
func readUserConfig() (userConfig, bool) {
	var uc userConfig
	b, err := os.ReadFile(configPath())
	if err != nil {
		return uc, false
	}
	if err := json.Unmarshal(b, &uc); err != nil {
		return userConfig{}, false
	}
	return uc, true
}

func writeUserConfig(uc userConfig) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(uc, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the token is a shared secret.
	return os.WriteFile(configPath(), b, 0o600)
}

// isOnboarded reports whether the user has completed the setup wizard.
func isOnboarded() bool {
	uc, ok := readUserConfig()
	return ok && uc.Onboarded
}

// providerIsPublic reports whether a provider exposes moona to the public
// internet (so it warrants an app token).
func providerIsPublic(provider string) bool {
	switch provider {
	case providerNgrok, providerCFNamed, providerQuick, "":
		return true
	default:
		return false
	}
}

// generateToken returns a URL-safe random token for app-level auth.
func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// rand.Read only fails on a broken platform RNG; fall back to a fixed-length
		// but still non-empty token rather than crash the wizard.
		return "moona-token"
	}
	return hex.EncodeToString(b)
}

// providerLabel is a short human name for a provider id, used in status lines.
func providerLabel(provider string) string {
	switch provider {
	case providerNgrok:
		return "ngrok"
	case providerCFNamed:
		return "Cloudflare named tunnel"
	case providerLocal:
		return "local network only"
	case providerQuick, "":
		return "Cloudflare Quick Tunnel"
	default:
		return provider
	}
}

// phoneConnectedOnce reports whether a phone has ever connected (so we can stop
// showing the connect panel on every session start).
func phoneConnectedOnce() bool {
	uc, _ := readUserConfig()
	return uc.PhoneConnectedOnce
}

// markPhoneConnectedOnce records that a phone has connected. Writes a config file
// even if none existed yet (a never-onboarded Quick Tunnel user still benefits).
func markPhoneConnectedOnce() {
	uc, _ := readUserConfig()
	if uc.PhoneConnectedOnce {
		return
	}
	uc.PhoneConnectedOnce = true
	_ = writeUserConfig(uc)
}

// tunnelEnabledFor reports whether a tunnel should be started for this provider.
// The --tunnel=false flag still overrides this to force local-only.
func tunnelEnabledFor(provider string) bool {
	return provider != providerLocal
}

// knownPublicURL returns the public URL that is known ahead of time from config,
// before the tunnel process actually connects. Fixed-hostname providers (a named
// Cloudflare tunnel, an ngrok reserved domain) have a stable URL derived straight
// from config, so we can advertise it immediately instead of making the first
// client race tunnel startup and fall back to the local-only URL. Ephemeral
// providers (Quick Tunnel) get a random URL only known once the tunnel is up, so
// this returns "" for them.
func knownPublicURL(uc userConfig) string {
	switch uc.TunnelProvider {
	case providerCFNamed:
		if uc.CFHostname != "" {
			return "https://" + uc.CFHostname
		}
	case providerNgrok:
		if uc.NgrokDomain != "" {
			return "https://" + uc.NgrokDomain
		}
	}
	return ""
}
