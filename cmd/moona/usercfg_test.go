package main

import "testing"

func TestUserConfigRoundtrip(t *testing.T) {
	// Isolate stateDir() to a temp LOCALAPPDATA so the test never touches the real
	// config.json (see the moona-tui-test-isolation note).
	t.Setenv("LOCALAPPDATA", t.TempDir())

	if _, ok := readUserConfig(); ok {
		t.Fatal("expected no config before writing")
	}
	if isOnboarded() {
		t.Fatal("should not be onboarded before writing")
	}

	want := userConfig{
		Onboarded:      true,
		TunnelProvider: providerNgrok,
		NgrokAuthtoken: "tok_123",
		NgrokDomain:    "moona.ngrok-free.app",
		Token:          "abcdef",
	}
	if err := writeUserConfig(want); err != nil {
		t.Fatalf("writeUserConfig: %v", err)
	}

	got, ok := readUserConfig()
	if !ok {
		t.Fatal("expected config to load after writing")
	}
	if got != want {
		t.Errorf("roundtrip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if !isOnboarded() {
		t.Error("should report onboarded after writing Onboarded=true")
	}
}

func TestReadUserConfigMissingIsNotOnboarded(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	uc, ok := readUserConfig()
	if ok {
		t.Error("missing config should report ok=false")
	}
	if uc.Onboarded {
		t.Error("zero config should not be onboarded")
	}
	// A never-onboarded install must behave like the legacy Quick Tunnel default.
	if !tunnelEnabledFor(uc.TunnelProvider) {
		t.Error("empty provider should still enable a tunnel (legacy default)")
	}
}
