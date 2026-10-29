package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/profiles"
)

func readInstructionStampVersion(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, profiles.GenerationStampFileName))
	if err != nil {
		t.Fatal(err)
	}
	var stamp struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &stamp); err != nil {
		t.Fatal(err)
	}
	return stamp.Version
}

func TestInstructionsRegenStampsActualRunningVersion(t *testing.T) {
	_, _ = sandboxInstructionsEnv(t)
	dir := profiles.DefaultDir()
	if _, err := profiles.Switch(dir, "localization", "v-old"); err != nil {
		t.Fatal(err)
	}
	cmd, _, _ := captureCmd()
	if err := runInstructionsRegen(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := readInstructionStampVersion(t, dir); got != canonicalVersion() {
		t.Fatalf("CLI omitted running version: %q want %q", got, canonicalVersion())
	}
	if got := profiles.ActiveName(dir); got != "localization" {
		t.Fatalf("regen changed stored selection: %q", got)
	}
}

func TestDaemonStartRefreshesManagedProfilesOnceAndPreservesConflicts(t *testing.T) {
	_, _ = sandboxInstructionsEnv(t)
	dir := profiles.DefaultDir()
	if _, err := profiles.Switch(dir, "localization", "v-old"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(profiles.ActiveEnv, "full")
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	refreshInstructionProfilesOnStart(dir, canonicalVersion(), logger)
	if got := readInstructionStampVersion(t, dir); got != canonicalVersion() {
		t.Fatalf("daemon did not stamp its actual version: %q", got)
	}
	localization, _ := profiles.ByName("localization")
	activeBefore, err := os.ReadFile(filepath.Join(dir, profiles.ActiveFileName))
	if err != nil || string(activeBefore) != localization.Body() {
		t.Fatal("daemon persisted environment override instead of stored selection")
	}
	stampBefore, err := os.ReadFile(filepath.Join(dir, profiles.GenerationStampFileName))
	if err != nil {
		t.Fatal(err)
	}
	refreshInstructionProfilesOnStart(dir, canonicalVersion(), logger)
	stampAfter, err := os.ReadFile(filepath.Join(dir, profiles.GenerationStampFileName))
	if err != nil || string(stampBefore) != string(stampAfter) {
		t.Fatal("second start changed generator metadata")
	}
	activeAfter, err := os.ReadFile(filepath.Join(dir, profiles.ActiveFileName))
	if err != nil || string(activeAfter) != string(activeBefore) {
		t.Fatal("second start changed active profile bytes")
	}
	if got := logs.FilterMessage("daemon: regenerated instruction profiles").Len(); got != 1 {
		t.Fatalf("startup refresh logged %d regeneration lines, want1", got)
	}
	const custom = "my custom instructions\n"
	if err := os.WriteFile(filepath.Join(dir, "core.md"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	refreshInstructionProfilesOnStart(dir, "v-next", logger)
	data, err := os.ReadFile(filepath.Join(dir, "core.md"))
	if err != nil || string(data) != custom {
		t.Fatal("startup overwrote user content")
	}
	warnings := logs.FilterMessage("daemon: could not refresh instruction profiles; custom files preserved").All()
	if len(warnings) != 1 || !strings.Contains(warnings[0].ContextMap()["error"].(string), "move it aside") {
		t.Fatalf("conflict was not actionable/nonfatal: %+v", warnings)
	}
}
