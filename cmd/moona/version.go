package main

import (
	"fmt"
	"runtime"
)

// Build metadata. These are overridden at release time via
// -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
// A plain `go build` leaves them at these defaults, which marks a dev build
// and disables the self-updater (there is no release to compare against).
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// isDevBuild reports whether this binary was built without release metadata.
// Dev builds skip both the startup auto-update and `moona update`.
func isDevBuild() bool {
	return version == "dev" || version == ""
}

func printVersion() {
	fmt.Printf("moona %s\n", version)
	if commit != "" {
		fmt.Printf("  commit: %s\n", commit)
	}
	if date != "" {
		fmt.Printf("  built:  %s\n", date)
	}
	fmt.Printf("  %s/%s %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
}
