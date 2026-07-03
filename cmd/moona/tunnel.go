package main

import (
	"archive/zip"
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

type tempTunnel struct {
	cmd  *exec.Cmd
	done chan error // receives the process's exit (from cmd.Wait) exactly once
}

// wait blocks until the tunnel helper process exits. Safe to call once on a
// tunnel returned from startTunnelProvider (that path leaves the exit result
// unconsumed for the supervisor).
func (t *tempTunnel) wait() {
	if t == nil || t.done == nil {
		return
	}
	<-t.done
}

// startBestTunnel tries each tunnel provider in order and returns the first one
// that yields a public URL. The caller decides whether tunnelling is enabled.
func startBestTunnel(port int) (*tempTunnel, string, error) {
	var failures []string
	for _, provider := range tunnelProviders(port) {
		terminalPrintln(os.Stderr, "starting temporary tunnel via ", provider.name, "...")
		tunnel, publicURL, err := startTunnelProvider(provider, 45*time.Second)
		if err != nil {
			failures = append(failures, provider.name+": "+err.Error())
			continue
		}
		terminalPrintln(os.Stderr, "tunnel ready via ", provider.name)
		return tunnel, publicURL, nil
	}
	return nil, "", fmt.Errorf("all tunnel providers failed: %s", strings.Join(failures, " | "))
}

type tunnelProvider struct {
	name    string
	binary  string
	args    []string
	env     []string // extra environment variables (KEY=VALUE) for the tunnel process
	prepare func() (string, error)
	// fixedURL, when non-empty, is the public URL to return once the tunnel is
	// established, rather than parsing it from output. Used by named tunnels
	// (Cloudflare) whose hostname is known ahead of time. Readiness is detected via
	// readyMarkers (lowercase substrings of a log line that mean "connected").
	fixedURL     string
	readyMarkers []string
}

// startConfiguredTunnel starts the tunnel for the user's chosen provider. Quick
// Tunnel (and an unset/legacy provider) keep the historical best-effort fallback
// chain so existing installs are unaffected. Local-only returns no tunnel.
func startConfiguredTunnel(port int, uc userConfig) (*tempTunnel, string, error) {
	switch uc.TunnelProvider {
	case providerLocal:
		return nil, "", nil
	case providerNgrok:
		return startSingleProvider(ngrokProvider(port, uc))
	case providerCFNamed:
		return startSingleProvider(cloudflareNamedProvider(port, uc))
	default: // providerQuick or "" (never onboarded)
		return startBestTunnel(port)
	}
}

// startSingleProvider starts exactly one provider and returns its error verbatim
// (no fallback), so a misconfigured ngrok/Cloudflare setup surfaces a clear
// message instead of silently degrading to a different URL the phone can't reach.
func startSingleProvider(provider tunnelProvider) (*tempTunnel, string, error) {
	terminalPrintln(os.Stderr, "starting tunnel via ", provider.name, "...")
	tunnel, publicURL, err := startTunnelProvider(provider, 60*time.Second)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", provider.name, err)
	}
	terminalPrintln(os.Stderr, "tunnel ready via ", provider.name)
	return tunnel, publicURL, nil
}

// ngrokProvider builds the ngrok command for a reserved static domain. The
// authtoken is passed via NGROK_AUTHTOKEN so it never appears in the process
// argument list. With a reserved --domain the URL is identical across restarts.
func ngrokProvider(port int, uc userConfig) tunnelProvider {
	args := []string{"http", "--log", "stdout", "--log-format", "logfmt"}
	if uc.NgrokDomain != "" {
		args = append(args, "--domain", uc.NgrokDomain)
	}
	args = append(args, fmt.Sprintf("http://127.0.0.1:%d", port))
	p := tunnelProvider{
		name:    "ngrok",
		binary:  "ngrok",
		prepare: ensureNgrok,
		args:    args,
	}
	if uc.NgrokAuthtoken != "" {
		p.env = []string{"NGROK_AUTHTOKEN=" + uc.NgrokAuthtoken}
	}
	if uc.NgrokDomain != "" {
		p.fixedURL = "https://" + strings.TrimPrefix(strings.TrimPrefix(uc.NgrokDomain, "https://"), "http://")
		// ngrok logs this once the tunnel is live; lets a custom reserved domain
		// (not a *.ngrok-free.app host findTunnelURL recognizes) still resolve.
		p.readyMarkers = []string{"started tunnel"}
	}
	return p
}

// cloudflareNamedProvider runs a pre-created Cloudflare named tunnel. The user
// routes their hostname to the tunnel during setup (cloudflared tunnel route
// dns); here we just run it and report the known hostname. Reuses the same
// cloudflared binary moona already downloads for Quick Tunnels.
func cloudflareNamedProvider(port int, uc userConfig) tunnelProvider {
	host := strings.TrimPrefix(strings.TrimPrefix(uc.CFHostname, "https://"), "http://")
	return tunnelProvider{
		name:     "Cloudflare named tunnel",
		binary:   "cloudflared",
		prepare:  ensureCloudflared,
		args:     []string{"tunnel", "--url", fmt.Sprintf("http://127.0.0.1:%d", port), "run", uc.CFTunnel},
		fixedURL: "https://" + host,
		// cloudflared logs one of these once an edge connection is actually live.
		readyMarkers: []string{"registered tunnel connection", "connection registered"},
	}
}

func tunnelProviders(port int) []tunnelProvider {
	return []tunnelProvider{
		{
			name:    "Cloudflare Quick Tunnel",
			binary:  "cloudflared",
			prepare: ensureCloudflared,
			args:    []string{"tunnel", "--url", fmt.Sprintf("http://127.0.0.1:%d", port)},
		},
		{
			name:   "Pinggy",
			binary: "ssh",
			args: []string{
				"-p", "443",
				"-o", "BatchMode=yes",
				"-o", "NumberOfPasswordPrompts=0",
				"-o", "ExitOnForwardFailure=yes",
				"-o", "StrictHostKeyChecking=accept-new",
				"-o", "ServerAliveInterval=30",
				"-R", fmt.Sprintf("0:127.0.0.1:%d", port),
				"qr@free.pinggy.io",
			},
		},
		{
			name:   "localhost.run",
			binary: "ssh",
			args: []string{
				"-o", "ExitOnForwardFailure=yes",
				"-o", "StrictHostKeyChecking=accept-new",
				"-o", "ServerAliveInterval=30",
				"-R", fmt.Sprintf("80:127.0.0.1:%d", port),
				"nokey@localhost.run",
			},
		},
	}
}
func resolveTunnelBinary(provider tunnelProvider) (string, error) {
	if provider.prepare != nil {
		return provider.prepare()
	}
	binaryPath, err := exec.LookPath(provider.binary)
	if err != nil {
		return "", fmt.Errorf("%s not found in PATH", provider.binary)
	}
	return binaryPath, nil
}

func ensureCloudflared() (string, error) {
	if cloudflaredPath, err := exec.LookPath("cloudflared"); err == nil {
		return cloudflaredPath, nil
	}
	if runtime.GOOS != "windows" {
		return "", errors.New("cloudflared not found in PATH")
	}

	cacheDir, err := os.UserCacheDir()
	if err != nil || cacheDir == "" {
		cacheDir = os.Getenv("LOCALAPPDATA")
	}
	if cacheDir == "" {
		return "", errors.New("cannot find a cache directory for cloudflared")
	}

	targetDir := filepath.Join(cacheDir, appName)
	targetPath := filepath.Join(targetDir, "cloudflared.exe")
	if info, err := os.Stat(targetPath); err == nil && !info.IsDir() {
		return targetPath, nil
	}

	downloadURL, err := cloudflaredDownloadURL()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", err
	}

	terminalPrintln(os.Stderr, "cloudflared not found; downloading Cloudflare Quick Tunnel helper...")
	tmpPath := targetPath + ".tmp"
	if err := downloadFile(downloadURL, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	_ = os.Chmod(targetPath, 0o755)
	return targetPath, nil
}

// ensureNgrok returns a path to the ngrok binary, downloading and extracting it
// on Windows if it is not already in PATH or the moona cache. ngrok ships as a
// zip (unlike cloudflared's bare .exe), so this unzips ngrok.exe out of it.
func ensureNgrok() (string, error) {
	if ngrokPath, err := exec.LookPath("ngrok"); err == nil {
		return ngrokPath, nil
	}
	if runtime.GOOS != "windows" {
		return "", errors.New("ngrok not found in PATH")
	}

	cacheDir, err := os.UserCacheDir()
	if err != nil || cacheDir == "" {
		cacheDir = os.Getenv("LOCALAPPDATA")
	}
	if cacheDir == "" {
		return "", errors.New("cannot find a cache directory for ngrok")
	}

	targetDir := filepath.Join(cacheDir, appName)
	targetPath := filepath.Join(targetDir, "ngrok.exe")
	if info, err := os.Stat(targetPath); err == nil && !info.IsDir() {
		return targetPath, nil
	}

	downloadURL, err := ngrokDownloadURL()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", err
	}

	terminalPrintln(os.Stderr, "ngrok not found; downloading ngrok helper...")
	zipPath := targetPath + ".zip"
	if err := downloadFile(downloadURL, zipPath); err != nil {
		_ = os.Remove(zipPath)
		return "", err
	}
	defer os.Remove(zipPath)
	if err := extractZipEntry(zipPath, "ngrok.exe", targetPath); err != nil {
		_ = os.Remove(targetPath)
		return "", err
	}
	_ = os.Chmod(targetPath, 0o755)
	return targetPath, nil
}

func ngrokDownloadURL() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64", "386":
		// ngrok uses Go architecture names in its Windows release assets.
	default:
		return "", fmt.Errorf("ngrok auto-download is unsupported on %s", runtime.GOARCH)
	}
	// Equinox app id bNyj1mQVY4c is ngrok's stable download channel.
	return "https://bin.equinox.io/c/bNyj1mQVY4c/ngrok-v3-stable-windows-" + runtime.GOARCH + ".zip", nil
}

// extractZipEntry writes the named file from a zip archive to destPath.
func extractZipEntry(zipPath, entryName, destPath string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if filepath.Base(f.Name) != entryName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		tmp := destPath + ".tmp"
		out, err := os.Create(tmp)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			_ = os.Remove(tmp)
			return err
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return os.Rename(tmp, destPath)
	}
	return fmt.Errorf("%s not found in %s", entryName, filepath.Base(zipPath))
}

func cloudflaredDownloadURL() (string, error) {
	suffix := runtime.GOARCH
	switch runtime.GOARCH {
	case "amd64", "arm64", "386":
		// Cloudflare uses Go architecture names in the Windows release assets.
	default:
		return "", fmt.Errorf("cloudflared auto-download is unsupported on %s", runtime.GOARCH)
	}
	return "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-" + suffix + ".exe", nil
}

func downloadFile(downloadURL, targetPath string) error {
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(downloadURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download %s failed: %s", downloadURL, resp.Status)
	}

	out, err := os.Create(targetPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func startTunnelProvider(provider tunnelProvider, timeout time.Duration) (*tempTunnel, string, error) {
	binaryPath, err := resolveTunnelBinary(provider)
	if err != nil {
		return nil, "", err
	}

	cmd := exec.Command(binaryPath, provider.args...)
	cmd.Stdin = strings.NewReader("\n")
	// Run the helper in its own process group with no inherited console, exactly
	// like the daemon itself. Without this the child sits in the daemon's process
	// group and a stray console-control event (Ctrl-Break to the group) can take it
	// down while the daemon lives on — stranding the public hostname.
	cmd.SysProcAttr = detachedSysProcAttr()
	if len(provider.env) > 0 {
		cmd.Env = append(os.Environ(), provider.env...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", err
	}

	var recentOutput []string
	lines := make(chan string, 32)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}

	scan := func(r io.Reader) {
		s := bufio.NewScanner(r)
		for s.Scan() {
			select {
			case lines <- s.Text():
			default:
			}
		}
	}
	go scan(stdout)
	go scan(stderr)
	go func() { done <- cmd.Wait() }()

	tunnel := &tempTunnel{cmd: cmd, done: done}
	urlRE := regexp.MustCompile(`https?://[^\s\]"'<>]+`)
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case line := <-lines:
			line = strings.TrimSpace(line)
			if line != "" {
				recentOutput = appendRecent(recentOutput, line, 8)
			}
			// Named tunnels don't print their public URL; return the known hostname
			// once the process reports an established connection.
			if provider.fixedURL != "" && lineHasReadyMarker(line, provider.readyMarkers) {
				return tunnel, provider.fixedURL, nil
			}
			if match := findTunnelURL(line, urlRE); match != "" {
				return tunnel, match, nil
			}
		case err := <-done:
			if err == nil {
				err = errors.New("tunnel exited before printing a URL")
			}
			return nil, "", fmt.Errorf("%w%s", err, formatRecentTunnelOutput(recentOutput))
		case <-timer.C:
			tunnel.stop()
			return nil, "", fmt.Errorf("timed out waiting for tunnel URL after %s%s", timeout.Round(time.Second), formatRecentTunnelOutput(recentOutput))
		}
	}
}

func findTunnelURL(line string, urlRE *regexp.Regexp) string {
	if line == "" {
		return ""
	}
	lowerLine := strings.ToLower(line)
	matches := urlRE.FindAllString(line, -1)
	for _, match := range matches {
		match = strings.TrimRight(match, ".,;)]}")
		u, err := url.Parse(match)
		if err != nil {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if host == "" || host == "twitter.com" || host == "localhost.run" || host == "admin.localhost.run" || host == "pinggy.io" || host == "developers.cloudflare.com" {
			continue
		}
		switch {
		case strings.HasSuffix(host, ".pinggy.link"):
			return match
		case strings.HasSuffix(host, ".trycloudflare.com"):
			return match
		case strings.HasSuffix(host, ".lhr.life"):
			return match
		case strings.HasSuffix(host, ".ngrok-free.app"),
			strings.HasSuffix(host, ".ngrok-free.dev"),
			strings.HasSuffix(host, ".ngrok.app"),
			strings.HasSuffix(host, ".ngrok.dev"),
			strings.HasSuffix(host, ".ngrok.io"):
			return match
		case strings.Contains(lowerLine, "tunneled with tls termination") && strings.HasSuffix(host, ".localhost.run"):
			return match
		}
	}
	return ""
}
func lineHasReadyMarker(line string, markers []string) bool {
	if line == "" || len(markers) == 0 {
		return false
	}
	lower := strings.ToLower(line)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func appendRecent(lines []string, line string, limit int) []string {
	lines = append(lines, line)
	if len(lines) > limit {
		copy(lines, lines[len(lines)-limit:])
		lines = lines[:limit]
	}
	return lines
}

func formatRecentTunnelOutput(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return "; last output: " + strings.Join(lines, " | ")
}

func (t *tempTunnel) stop() {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return
	}
	// Kill only; the cmd.Wait goroutine started in startTunnelProvider reaps the
	// process and delivers the result to done (consumed by wait()). Calling Wait
	// here too would be a double-wait and could deadlock shutdown against wait().
	_ = t.cmd.Process.Kill()
}
