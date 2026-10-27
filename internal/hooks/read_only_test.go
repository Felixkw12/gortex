package hooks

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

func TestFileIndexStatusReadOnlyPrecedesIndexed(t *testing.T) {
	st := fileIndexStatusFromCoverage(daemon.FileCoverageResult{
		Covered: true, Symbols: 7, Answered: true, Tracked: true,
		MutationState: "read_only", MutationReason: "checkout route changed",
	})
	if !st.ReadOnly || st.Indexed || !st.ProbeOK || !st.noGraphAnswer() || st.MutationReason != "checkout route changed" {
		t.Fatalf("refusal must take precedence over retained coverage: %+v", st)
	}
	ready := fileIndexStatusFromCoverage(daemon.FileCoverageResult{Covered: true, Symbols: 7, Answered: true, Tracked: true})
	if ready.ReadOnly || !ready.Indexed || ready.noGraphAnswer() {
		t.Fatalf("ready coverage changed: %+v", ready)
	}
	if !((fileIndexStatus{ReadOnly: true, Indexed: true}).noGraphAnswer()) {
		t.Fatal("independent read-only fact must precede indexed")
	}
}

func TestReadOnlyNativeDoorsKeepOneProbeAndRecoveryAcrossModes(t *testing.T) {
	for _, blocking := range []string{"", "1"} {
		t.Run("block_edit="+blocking, func(t *testing.T) {
			t.Setenv(editBlockingEnvVar, blocking)
			probes := 0
			stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus {
				probes++
				return fileIndexStatus{ReadOnly: true, Indexed: true, Tracked: true, ProbeOK: true}
			})
			for _, door := range []struct {
				name string
				call func() enrichResult
			}{
				{"Read", func() enrichResult { return enrichRead(map[string]any{"file_path": "a.go"}, "/repo") }},
				{"cat", func() enrichResult { return enrichBash(map[string]any{"command": "cat a.go"}, "/repo") }},
				{"head", func() enrichResult { return enrichBash(map[string]any{"command": "head a.go"}, "/repo") }},
				{"tail", func() enrichResult { return enrichBash(map[string]any{"command": "tail a.go"}, "/repo") }},
				{"Edit", func() enrichResult { return enrichEdit(map[string]any{"file_path": "a.go"}, "/repo") }},
				{"Write", func() enrichResult { return enrichWrite(map[string]any{"file_path": "a.go"}, "/repo") }},
			} {
				t.Run(door.name, func(t *testing.T) {
					before := probes
					got := door.call()
					if probes != before+1 {
						t.Fatalf("coverage probes=%d, want one", probes-before)
					}
					for _, mode := range []Mode{ModeDeny, ModeEnrich, ModeConsultUnlock, ModeAdaptiveNudge} {
						result := applyMode(HookInput{ToolName: door.name}, false, mode, got)
						if result.deny || !strings.Contains(result.context, "fresh view") || !strings.Contains(result.context, "reconnect the MCP session") || strings.Contains(strings.ToLower(result.context), "stop") {
							t.Fatalf("mode=%s: native recovery path unavailable: %+v", mode, result)
						}
						if (door.name == "Edit" || door.name == "Write") && !strings.Contains(result.context, "git diff and the tests") {
							t.Fatalf("native write lacks verification: %+v", result)
						}
					}
				})
			}
		})
	}
}
