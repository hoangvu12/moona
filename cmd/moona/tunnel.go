package main

import (
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
	cmd *exec.Cmd
}

func maybeStartTunnel(running *runningShare) error {
	if !running.cfg.tunnel {
		return nil
	}

	var failures []string
	for _, provider := range tunnelProviders(running.cfg.port) {
		terminalPrintln(os.Stderr, "starting temporary tunnel via ", provider.name, "...")
		tunnel, publicURL, err := startTunnelProvider(provider, 45*time.Second)
		if err != nil {
			failures = append(failures, provider.name+": "+err.Error())
			continue
		}
		running.tunnel = tunnel
		running.publicURL = publicURL
		terminalPrintln(os.Stderr, "tunnel ready via ", provider.name)
		return nil
	}

	return fmt.Errorf("all tunnel providers failed: %s", strings.Join(failures, " | "))
}

type tunnelProvider struct {
	name    string
	binary  string
	args    []string
	prepare func() (string, error)
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

	tunnel := &tempTunnel{cmd: cmd}
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
		case strings.Contains(lowerLine, "tunneled with tls termination") && strings.HasSuffix(host, ".localhost.run"):
			return match
		}
	}
	return ""
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
	_ = t.cmd.Process.Kill()
	_, _ = t.cmd.Process.Wait()
}
