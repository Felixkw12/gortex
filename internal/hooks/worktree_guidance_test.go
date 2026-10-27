package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

func TestExternalAgentHintKeepsUnregisteredConfiguredFamilyPending(t *testing.T) {
	primary, linked := filepath.Join(t.TempDir(), "primary"), filepath.Join(t.TempDir(), "linked")
	if err := os.Mkdir(primary, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionGuidanceGit(t, primary, "init", "-q", "-b", "main")
	sessionGuidanceGit(t, primary, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "fixture")
	sessionGuidanceGit(t, primary, "worktree", "add", "-q", "-b", "feature", linked)
	oldStatus, oldRoots := sessionStartStatusFn, hookConfiguredRepoRoots
	t.Cleanup(func() { sessionStartStatusFn, hookConfiguredRepoRoots = oldStatus, oldRoots })
	sessionStartStatusFn = func() (*daemon.StatusResponse, error) { return &daemon.StatusResponse{Ready: true}, nil }
	hookConfiguredRepoRoots = func() []string { return []string{primary} }
	got := buildStaleIndexHint(linked)
	if !strings.Contains(got, "checkout discovery is pending") || !strings.Contains(got, "view_building") || strings.Contains(got, "gortex track "+linked) {
		t.Fatalf("configured family was told to retrack: %s", got)
	}
	if strings.Contains(strings.ReplaceAll(got, "no reconnect is needed", ""), "reconnect") {
		t.Fatalf("external hook gave a reconnect instruction: %s", got)
	}
	outside := t.TempDir()
	if got := buildStaleIndexHint(outside); !strings.Contains(got, "gortex track "+outside) {
		t.Fatalf("unrelated directory lost explicit track guidance: %s", got)
	}
}
