package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

func TestDaemonIdentityReplacementDirection(t *testing.T) {
	for _, tc := range []struct {
		name, version, local string
		protocol             int
		known, older         bool
	}{
		{"older protocol without release", "", "v0.64.12", 0, true, true},
		{"missing protocol is not zero", "", "v0.64.12", 0, false, false},
		{"older release", "v0.64.10", "v0.64.12", daemon.ProtocolVersion, true, true},
		{"newer protocol vetoes older release", "v0.64.10", "v0.64.12", daemon.ProtocolVersion + 1, true, false},
		{"newer release vetoes older protocol", "v0.64.13", "v0.64.12", 0, true, false},
		{"same", "v0.64.12", "v0.64.12", daemon.ProtocolVersion, true, false},
		{"unknown versions", "garbage", "v0.64.12", daemon.ProtocolVersion, true, false},
		{"development release has no direction", "v0.64.10", "v0.0.0-dev", daemon.ProtocolVersion, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := daemonIdentity{version: tc.version, protocol: tc.protocol, protocolKnown: tc.known}
			if got := d.olderThan(tc.local); got != tc.older {
				t.Fatalf("olderThan(%q) = %v", tc.local, got)
			}
		})
	}
}

func TestDaemonMismatchDiagnosticNamesBothIdentitiesAndDirection(t *testing.T) {
	oldVersion := binaryVersion
	binaryVersion = func() string { return "v0.64.12" }
	t.Cleanup(func() { binaryVersion = oldVersion })
	for _, tc := range []struct {
		version  string
		protocol int
		remedy   string
	}{
		{"v0.64.10", 0, "gortex daemon restart"},
		{"v0.64.10", daemon.ProtocolVersion + 1, "this CLI is older than the running daemon; use the matching binary"},
		{"v0.64.13", 0, "this CLI is older than the running daemon; use the matching binary"},
	} {
		err := daemonMismatchError(daemonIdentity{version: tc.version, protocol: tc.protocol, protocolKnown: true})
		if !errors.Is(err, daemon.ErrProtocolVersionMismatch) {
			t.Fatalf("diagnostic lost sentinel: %v", err)
		}
		for _, want := range []string{tc.version, "v0.64.12", "protocol", tc.remedy} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%q missing %q", err, want)
			}
		}
	}
}

func TestEnsureDaemonCompatibleStopFailureKeepsMCPPolicy(t *testing.T) {
	f := installStaleDaemonFixture(t, "v0.64.10", "v0.64.12")
	stopStaleDaemon = func(daemonIdentity) error { return errors.New("controlled compatible stop failure") }
	decision, observed, err := ensureDaemonReadyWithError(true)
	if decision != daemonReady || err != nil || observed.mismatch() || f.spawns.Load() != 0 {
		t.Fatalf("compatible failure changed startup policy: %d, %+v, %v", decision, observed, err)
	}
}

func TestEnsureDaemonMismatchStopFailureRemainsTerminal(t *testing.T) {
	f := installStaleDaemonFixture(t, "v0.64.10", "v0.64.12")
	old := daemonIdentity{version: "v0.64.10", protocol: 0, protocolKnown: true, err: &daemon.ProtocolMismatchError{DaemonVersion: "v0.64.10", DaemonProtocolKnown: true, ClientProtocol: daemon.ProtocolVersion}}
	runningDaemonInfo = func() daemonIdentity { return old }
	stopStaleDaemon = func(daemonIdentity) error { return errors.New("controlled mismatch stop failure") }
	_, observed, err := ensureDaemonReadyWithError(true)
	if !errors.Is(err, daemon.ErrProtocolVersionMismatch) || !observed.mismatch() || f.spawns.Load() != 0 {
		t.Fatalf("mismatch stop failure lost refusal: %+v, %v", observed, err)
	}
}
