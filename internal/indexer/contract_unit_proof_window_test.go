package indexer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A later reader's work cannot extend an earlier reader's opening/closing
// stamp bracket. A real write after one reader closes is still observed by
// the remaining readers and final full fence; no equal torn answer is blessed.
func TestContractUnitReplayClosesEachProofBeforeLaterReaders(t *testing.T) {
	for _, changeAfterFirst := range []bool{false, true} {
		name := "unchanged_owner"
		if changeAfterFirst {
			name = "owner_changes_after_first_proof"
		}
		t.Run(name, func(t *testing.T) {
			f, files := newOwnerReadersFixture(t)
			const owner = "fixture/consts.go"
			staged, wrote := false, false
			checked := map[string]bool{}
			closingAfterEarlierReader := false
			f.onWait = func() {
				if staged {
					f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
				}
			}
			hooks := &contractConvergenceHooks{
				afterExtract: func(path string) {
					if !staged && path == "fixture/r0.go" {
						staged = true
						f.stageInFlight(owner)
					}
				},
				beforeRevalidateR1: func() {
					if len(checked) > 0 && len(checked) < 10 {
						closingAfterEarlierReader = true
					}
				},
				revalidated: func(path string, equal bool) {
					if !strings.HasPrefix(path, "fixture/r") {
						return
					}
					require.True(t, equal)
					checked[path] = true
					if changeAfterFirst && !wrote {
						wrote = true
						files["consts.go"] = ownerConsts + "// actual later same-contract source write\n"
						f.saveInFlight("consts.go", files["consts.go"])
					}
				},
			}
			report, err := f.job(hooks, nil)
			f.onWait = nil
			require.NoError(t, err)
			require.True(t, report.Published)
			require.True(t, staged)
			require.Len(t, checked, 10)
			require.True(t, closingAfterEarlierReader, "each reader closes its own proof before later readers are asked")
			require.Equal(t, changeAfterFirst, wrote)
			want := coldContracts(t, files)
			require.Equal(t, contractIDs(want), contractIDs(f.contracts()))
			require.Equal(t, contractMeta(want), contractMeta(f.contracts()))
		})
	}
}
