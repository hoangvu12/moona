package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/creativeprojects/go-selfupdate"
)

// repoSlug is the GitHub "owner/repo" the updater pulls releases from.
const repoSlug = "hoangvu12/moona"

// reexecEnv marks a process we spawned after auto-updating, so the freshly
// installed binary does not check for updates again and relaunch forever.
const reexecEnv = "MOONA_UPDATED"

// disableEnv lets users opt out of the startup update check entirely.
const disableEnv = "MOONA_NO_UPDATE"

// updateCheckInterval throttles the startup check so most launches stay instant.
const updateCheckInterval = 24 * time.Hour

// newUpdater builds an updater that verifies every download against the
// checksums.txt GoReleaser publishes alongside the archives.
func newUpdater() (*selfupdate.Updater, error) {
	return selfupdate.NewUpdater(selfupdate.Config{
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"},
	})
}

func detectLatest(ctx context.Context) (*selfupdate.Release, bool, error) {
	up, err := newUpdater()
	if err != nil {
		return nil, false, err
	}
	return up.DetectLatest(ctx, selfupdate.ParseSlug(repoSlug))
}

// runUpdate implements `moona update`. With --check it only reports whether a
// newer version exists without touching the binary.
func runUpdate(args []string) error {
	checkOnly := false
	for _, a := range args {
		switch a {
		case "--check":
			checkOnly = true
		case "-h", "--help":
			fmt.Println("Usage: moona update [--check]\n\n  --check   report whether an update is available without installing it")
			return nil
		default:
			return fmt.Errorf("unknown flag for update: %s", a)
		}
	}

	if isDevBuild() {
		fmt.Println("moona: this is a dev build; install a released version to enable updates")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	latest, found, err := detectLatest(ctx)
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}
	if !found {
		return errors.New("no matching release found for this platform")
	}
	if latest.LessOrEqual(version) {
		fmt.Printf("moona %s is already the latest version\n", version)
		return nil
	}

	if checkOnly {
		fmt.Printf("moona %s is available (current %s) — run 'moona update' to install\n", latest.Version(), version)
		return nil
	}

	exe, err := currentExe()
	if err != nil {
		return err
	}

	up, err := newUpdater()
	if err != nil {
		return err
	}
	fmt.Printf("moona: updating %s -> %s ...\n", version, latest.Version())
	if err := up.UpdateTo(ctx, latest, exe); err != nil {
		return fmt.Errorf("updating binary: %w", err)
	}
	fmt.Printf("moona: updated to %s\n", latest.Version())
	return nil
}

// maybeAutoUpdate runs just before a session starts. When enabled it checks
// GitHub at most once per day; if a newer release exists it verifies, swaps the
// running binary, and re-execs the same command so the user transparently runs
// the new version. Every failure path fails open: startup continues on the
// current binary. argv is the full argument list (e.g. ["share", "--port", ...])
// so the relaunched process behaves identically.
func maybeAutoUpdate(argv []string) {
	if isDevBuild() {
		return
	}
	if os.Getenv(reexecEnv) != "" {
		return // this is the freshly-updated child; do not loop
	}
	if truthyEnv(disableEnv) {
		return
	}
	if !dueForCheck() {
		return
	}
	// Record the attempt up front so transient failures don't cause a check on
	// every single launch.
	markChecked()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	latest, found, err := detectLatest(ctx)
	if err != nil || !found || latest.LessOrEqual(version) {
		return
	}

	exe, err := currentExe()
	if err != nil {
		return
	}

	up, err := newUpdater()
	if err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "moona: updating %s -> %s ...\n", version, latest.Version())
	dlCtx, dlCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer dlCancel()
	if err := up.UpdateTo(dlCtx, latest, exe); err != nil {
		fmt.Fprintf(os.Stderr, "moona: update failed (%v); continuing on %s\n", err, version)
		return
	}
	fmt.Fprintf(os.Stderr, "moona: updated to %s, relaunching...\n", latest.Version())
	reexec(exe, argv)
}

// reexec runs the (now updated) binary with the original arguments, proxying
// stdio and the exit code, then exits. It sets reexecEnv so the child skips the
// update check.
func reexec(exe string, argv []string) {
	cmd := exec.Command(exe, argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), reexecEnv+"=1")
	err := cmd.Run()
	if err == nil {
		os.Exit(0)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	fmt.Fprintln(os.Stderr, "moona: relaunch failed:", err)
	os.Exit(1)
}

// currentExe resolves the real path of the running executable, following any
// symlinks so the swap targets the actual file.
func currentExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

func updateStampPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, appName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "last-update-check"), nil
}

// dueForCheck reports whether at least updateCheckInterval has elapsed since the
// last check. If the stamp location is unavailable it returns false so we err
// toward not checking on every launch.
func dueForCheck() bool {
	p, err := updateStampPath()
	if err != nil {
		return false
	}
	info, err := os.Stat(p)
	if err != nil {
		return true // never checked
	}
	return time.Since(info.ModTime()) >= updateCheckInterval
}

func markChecked() {
	p, err := updateStampPath()
	if err != nil {
		return
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		_ = f.Close()
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)
}

func truthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
