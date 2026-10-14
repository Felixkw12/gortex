package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// contractLaneMutation runs one mutation body through the Indexer's own lane
// or the MultiIndexer's repository lane of the fixture repository, and
// overtakes it, when asked, with a newer mutation of the same owner admitted
// as the body ends.
type contractLaneMutation struct {
	f    *contractJobFixture
	idx  *Indexer
	lane string
}

func newContractLaneMutation(t *testing.T, f *contractJobFixture, lane string) *contractLaneMutation {
	t.Helper()
	f.mi.mu.RLock()
	idx := f.mi.indexers["fixture"]
	f.mi.mu.RUnlock()
	require.NotNil(t, idx)
	return &contractLaneMutation{f: f, idx: idx, lane: lane}
}

// overtake admits a newer mutation of the lane's owner and waits until the
// running one is superseded; the returned func abandons the newer receipt.
func (m *contractLaneMutation) overtake() func() {
	authority, target := m.idx.outputGenerationAuthority(), m.idx.legacyOutputTarget()
	if m.lane == "multi_indexer" {
		authority = m.f.mi.outputGenerationAuthority()
		target = legacyOutputTargetFor(outputStoreIdentity(m.f.mi.graph), "fixture", m.f.mi.repoRootPath("fixture"), "prefix:fixture")
	}
	newer := make(chan *OutputMutationReceipt, 1)
	issued := authority.Stats().Issued
	go func() {
		receipt, err := authority.Begin(context.Background(), OutputEntryIndexFile, target)
		if err != nil {
			receipt = nil
		}
		newer <- receipt
	}()
	for deadline := time.Now().Add(10 * time.Second); authority.Stats().Issued == issued && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	return func() {
		if receipt := <-newer; receipt != nil {
			receipt.Abandon()
		}
	}
}

func (m *contractLaneMutation) run(ctx context.Context, body func(func(*OutputSourceContent)) error) error {
	if m.lane == "multi_indexer" {
		return m.f.mi.withRepositoryOutputGenerationSource(ctx, "fixture", OutputEntryRepositoryReconcileLane, body)
	}
	return m.idx.withOutputGenerationSource(ctx, OutputEntryRepositoryReconcileLane, body)
}

// patch rewrites the fixture file name and applies it in the lane's body.
func (m *contractLaneMutation) patch(t *testing.T, name, text string) (string, func() error) {
	t.Helper()
	path := filepath.Join(m.f.root, name)
	writeFile(t, path, text)
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, future, future))
	return path, func() error {
		_, err := m.idx.incrementalPointWatcherPath(m.idx.rootPath, path)
		return err
	}
}

// TestContractOvertakenApplySettlesInOneEvent: a mutation overtaken after
// its body completed every core write settles its receipts before closing
// their windows, on both lanes. The settle closes the windows it settles, so
// the close and the settled flip land as one receipt event, and a contract
// job woken by it reads the final stamp: it asks once and publishes. The close
// after the settle closes only a window the settle could not, one whose path a
// later receipt write moved (here, a refused one), and a full scan taken
// during the body is never reused for the settled state.
func TestContractOvertakenApplySettlesInOneEvent(t *testing.T) {
	const owner = "fixture/consts.go"
	for _, lane := range []string{"indexer", "multi_indexer"} {
		for _, row := range []struct {
			name string
			// moved refuses a receipt write of the path after the staging:
			// the seq moves past the one the mutation staged, its window
			// stays open.
			moved bool
		}{{"settled_and_closed", false}, {"seq_moved_under_its_window", true}} {
			t.Run(lane+"/"+row.name, func(t *testing.T) {
				f := newRouteConstantFixture(t)
				m := newContractLaneMutation(t, f, lane)
				consts := jobConsts + "// overtaken\n"
				_, apply := m.patch(t, "consts.go", consts)
				scanner := newFixtureScanner(t, f)
				staged, proceed := make(chan struct{}), make(chan struct{})
				var during ContractFollowupScan
				var eventsBefore uint64
				var abandon func()
				key, found, err := f.store.ContractInputStateContext(t.Context(), "fixture", "")
				require.NoError(t, err)
				require.True(t, found)
				body := func(func(*OutputSourceContent)) error {
					if err := apply(); err != nil {
						return err
					}
					// The apply's receipt and window move, the attachment key
					// does not.
					f.restoreKey(key)
					if row.moved {
						current, _, err := f.store.ContractBoundaryReceiptContext(context.Background(), "fixture", "", owner)
						if err != nil {
							return err
						}
						stale := *current
						stale.Fingerprint = "refused"
						if err := f.store.AcceptContractBoundaryReceiptsContext(context.Background(), []graph.ContractBoundaryReceipt{stale}); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
							t.Errorf("an accept of another row is refused: %v", err)
						}
					}
					var err error
					during, err = scanner.scan(context.Background(), nil)
					if err != nil {
						return err
					}
					eventsBefore, _ = f.store.ContractReceiptEvents()
					close(staged)
					<-proceed
					abandon = m.overtake()
					return nil
				}
				done := make(chan error, 1)
				var woke, asks int
				var atWake ContractReceiptStamp
				if row.moved {
					close(proceed)
					done <- m.run(t.Context(), body)
				} else {
					// The job is captured before the apply begins and runs
					// while its window is open; the apply is overtaken while
					// the job waits on it.
					f.adjust = func(*ContractFollowupSnapshot) {
						go func() { done <- m.run(context.Background(), body) }()
						<-staged
					}
					f.onWait = func() {
						select {
						case <-proceed:
						default:
							close(proceed)
						}
					}
					hooks := &contractConvergenceHooks{
						roundWait: 5 * time.Second,
						woke: func() {
							woke++
							scan, err := newFixtureScanner(t, f).scan(context.Background(), []string{owner})
							if err == nil {
								atWake = scan.Stamps[owner]
							}
						},
						beforeAsk: func() { asks++ },
					}
					report, err := f.job(hooks, contractJobDeadline(time.Minute))
					f.onWait = nil
					require.NoError(t, err)
					require.True(t, report.Published)
					require.Equal(t, 1, woke, "the overtaken apply's end woke the job once")
					require.Equal(t, 1, asks, "the woken round asked once")
					require.True(t, atWake.Settled && !atWake.Writing, "the job woke to the final stamp: %+v", atWake)
					require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": consts, "helper.go": jobHelper})), contractIDs(f.contracts()))
				}
				err = <-done
				if abandon != nil {
					abandon()
				}
				require.ErrorIs(t, err, ErrOutputMutationReceiptSuperseded)
				require.True(t, during.Stamps[owner].Writing, "the body's windows were open as it ended")
				eventsAfter, _ := f.store.ContractReceiptEvents()
				require.Equal(t, eventsBefore+1, eventsAfter, "the overtaken apply's end is one receipt event")
				final, err := newFixtureScanner(t, f).scan(t.Context(), []string{owner})
				require.NoError(t, err)
				stamp := final.Stamps[owner]
				require.False(t, stamp.Accepted, "an overtaken apply is never accepted")
				require.False(t, stamp.Writing, "its window closed")
				require.True(t, stamp.quiescent())
				require.Equal(t, !row.moved, stamp.Settled, "it settled unless a later receipt write moved its seq")
				reused, err := scanner.scan(t.Context(), nil)
				require.NoError(t, err)
				require.Equal(t, stamp, reused.Stamps[owner], "a full scan after the end sees it, not the scan taken during the body")
			})
		}
	}
}
