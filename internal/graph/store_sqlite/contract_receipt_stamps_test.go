package store_sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestContractReceiptStampsAndWriteSequence pins the stamp scan's store
// half: payload-free rows by range and by point, and a write sequence that
// lives on the shared core, so a write through one generation handle is
// visible through every other handle's sample and seqs.
func TestContractReceiptStampsAndWriteSequence(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	writer, reader := s.AtGeneration(0), s.AtGeneration(0)
	a, b := boundaryStorageReceipt("a.go", "a1"), boundaryStorageReceipt("b.go", "b1")
	if err := writer.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a, b}); err != nil {
		t.Fatal(err)
	}
	if begun, done := reader.ContractReceiptWriteSample("repo"); begun != 1 || done != 1 {
		t.Fatalf("one settled write: begun=%d done=%d", begun, done)
	}
	if seqs := reader.ContractReceiptWriteSeqs("repo", "", []string{a.FilePath, b.FilePath, "repo/none.go"}); seqs[a.FilePath] != 1 || seqs[b.FilePath] != 1 || seqs["repo/none.go"] != 0 {
		t.Fatalf("seqs=%v", seqs)
	}
	all, err := reader.ContractBoundaryReceiptStampsContext(ctx, "repo", "", nil)
	if err != nil || len(all) != 2 || all[a.FilePath].Fingerprint != "a1" || all[a.FilePath].Accepted {
		t.Fatalf("range stamps=%v err=%v", all, err)
	}
	if err := writer.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a}); err != nil {
		t.Fatal(err)
	}
	point, err := reader.ContractBoundaryReceiptStampsContext(ctx, "repo", "", []string{a.FilePath, a.FilePath, "repo/none.go"})
	if err != nil || len(point) != 1 || !point[a.FilePath].Accepted {
		t.Fatalf("point stamps=%v err=%v", point, err)
	}
	// Accepting is a write too: a same-identity transition bumps the seq.
	if seqs := reader.ContractReceiptWriteSeqs("repo", "", []string{a.FilePath, b.FilePath}); seqs[a.FilePath] != 2 || seqs[b.FilePath] != 1 {
		t.Fatalf("seqs after accept=%v", seqs)
	}
	if begun, done := reader.ContractReceiptWriteSample("repo"); begun != 2 || done != 2 {
		t.Fatalf("bracket after accept: begun=%d done=%d", begun, done)
	}
	if begun, done := reader.ContractReceiptWriteSample("other"); begun != 0 || done != 0 {
		t.Fatalf("an untouched repository has an empty bracket: %d %d", begun, done)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := reader.ContractBoundaryReceiptStampsContext(canceled, "repo", "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan=%v", err)
	}
}

// TestContractReceiptSettleAndEvents pins the settle half of the write
// sequence: a receipt is settled only at the write seq its mutation wrote, a
// later write of the path unsettles it, and every ended write and settle
// advances the event counter and closes the channel a waiter holds.
func TestContractReceiptSettleAndEvents(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	writer, reader := s.AtGeneration(0), s.AtGeneration(0)
	events, changed := reader.ContractReceiptEvents()
	a := boundaryStorageReceipt("a.go", "a1")
	if err := writer.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("an ended receipt write closes the waiter's channel")
	}
	afterWrite, _ := reader.ContractReceiptEvents()
	if afterWrite != events+1 {
		t.Fatalf("events after a write: %d, want %d", afterWrite, events+1)
	}
	seqs := writer.ContractReceiptWriteSeqs("repo", "", []string{a.FilePath})
	// A stale seq settles nothing and announces nothing.
	writer.SettleContractReceiptWrites("repo", "", map[string]uint64{a.FilePath: seqs[a.FilePath] + 1})
	if state := reader.ContractReceiptWriteStates("repo", "", []string{a.FilePath})[a.FilePath]; state.Settled {
		t.Fatalf("settled at a seq the path was never written at: %+v", state)
	}
	if now, _ := reader.ContractReceiptEvents(); now != afterWrite {
		t.Fatalf("a settle that matched nothing is not an event: %d", now)
	}
	_, changed = reader.ContractReceiptEvents()
	writer.SettleContractReceiptWrites("repo", "", seqs)
	state := reader.ContractReceiptWriteStates("repo", "", []string{a.FilePath, "repo/none.go"})
	if !state[a.FilePath].Settled || state[a.FilePath].Seq != seqs[a.FilePath] || state["repo/none.go"].Settled {
		t.Fatalf("states after settle: %+v", state)
	}
	select {
	case <-changed:
	default:
		t.Fatal("a settle closes the waiter's channel")
	}
	// The next write of the path is in flight again until it settles or is
	// accepted.
	if err := writer.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a}); err != nil {
		t.Fatal(err)
	}
	if state := reader.ContractReceiptWriteStates("repo", "", []string{a.FilePath})[a.FilePath]; state.Settled || state.Seq != seqs[a.FilePath]+1 {
		t.Fatalf("a later write unsettles the path: %+v", state)
	}
}

// TestContractReceiptCoreWriteWindows pins the core-write window: a staging
// write of unaccepted rows opens it as it begins; it closes when its
// mutation's body ends at the seq it staged, when the staged row is accepted,
// or when its mutation settles it. Ending at a stale seq or a refused accept
// closes nothing, and an ending steps the repository's bracket and wakes
// waiters, as a receipt write does.
func TestContractReceiptCoreWriteWindows(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	writer, reader := s.AtGeneration(0), s.AtGeneration(0)
	state := func(path string) ContractReceiptWriteState {
		return reader.ContractReceiptWriteStates("repo", "", []string{path})[path]
	}
	a, b := boundaryStorageReceipt("a.go", "a1"), boundaryStorageReceipt("b.go", "b1")
	if st := state(a.FilePath); st.Writing || st.Seq != 0 {
		t.Fatalf("a path never written has no window: %+v", st)
	}
	if err := writer.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a, b}); err != nil {
		t.Fatal(err)
	}
	if !state(a.FilePath).Writing || !state(b.FilePath).Writing {
		t.Fatal("staging unaccepted rows opens their windows")
	}
	seqs := writer.ContractReceiptWriteSeqs("repo", "", []string{a.FilePath})
	events, changed := reader.ContractReceiptEvents()
	begun, done := reader.ContractReceiptWriteSample("repo")
	writer.EndContractCoreWrites("repo", "", map[string]uint64{a.FilePath: seqs[a.FilePath] + 1})
	if !state(a.FilePath).Writing {
		t.Fatal("an ending at a seq the path was not staged at closes nothing")
	}
	if now, _ := reader.ContractReceiptEvents(); now != events {
		t.Fatalf("an ending that closed nothing is not an event: %d", now)
	}
	writer.EndContractCoreWrites("repo", "", seqs)
	if st := state(a.FilePath); st.Writing || st.Settled || st.Seq != seqs[a.FilePath] {
		t.Fatalf("the body's ending closes the window and moves nothing else: %+v", st)
	}
	select {
	case <-changed:
	default:
		t.Fatal("an ending wakes the waiters")
	}
	if nb, nd := reader.ContractReceiptWriteSample("repo"); nb != begun+1 || nd != done+1 {
		t.Fatalf("an ending steps the bracket: %d/%d -> %d/%d", begun, done, nb, nd)
	}
	// A refused accept leaves the window open; the accept closes it.
	if err := writer.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a}); err != nil {
		t.Fatal(err)
	}
	if !state(a.FilePath).Writing {
		t.Fatal("staging the path again opens a new window")
	}
	stale := boundaryStorageReceipt("a.go", "a2")
	if err := writer.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{stale}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("accept of a row other than the staged one: %v", err)
	}
	if !state(a.FilePath).Writing {
		t.Fatal("a refused accept closes nothing")
	}
	if err := writer.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{a}); err != nil {
		t.Fatal(err)
	}
	if state(a.FilePath).Writing {
		t.Fatal("the accept closes the window")
	}
	// A settle closes the window at the seq it settles.
	if !state(b.FilePath).Writing {
		t.Fatal("b's window is still open")
	}
	writer.SettleContractReceiptWrites("repo", "", writer.ContractReceiptWriteSeqs("repo", "", []string{b.FilePath}))
	if st := state(b.FilePath); st.Writing || !st.Settled {
		t.Fatalf("a settle closes the window: %+v", st)
	}
}

// TestContractSeedQueriesSelectOneRepositoryErrorfully pins the enrichment
// round's seed reads: the repository's provides/consumes owner edges (by
// source node repository) and its Spring reader and Java method nodes, with
// errors returned rather than swallowed.
func TestContractSeedQueriesSelectOneRepositoryErrorfully(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	nodes := []*graph.Node{
		{ID: "repo/Clocks.java::Clocks", Kind: graph.KindType, Name: "Clocks", FilePath: "repo/Clocks.java", RepoPrefix: "repo", Language: "java"},
		{ID: "repo/Clocks.java::Clocks.systemClock", Kind: graph.KindMethod, Name: "systemClock", FilePath: "repo/Clocks.java", RepoPrefix: "repo", Language: "java"},
		{ID: "repo/Settings.java::Settings.url", Kind: graph.KindField, Name: "url", FilePath: "repo/Settings.java", RepoPrefix: "repo", Language: "java", Meta: map[string]any{"spring_config_keys": []string{"app.url"}}},
		{ID: "repo/plain.go::Plain", Kind: graph.KindFunction, Name: "Plain", FilePath: "repo/plain.go", RepoPrefix: "repo", Language: "go"},
		{ID: "other/Other.java::Other.run", Kind: graph.KindMethod, Name: "run", FilePath: "other/Other.java", RepoPrefix: "other", Language: "java"},
	}
	edges := []*graph.Edge{
		{From: "repo/Clocks.java::Clocks", To: "repo/Clocks.java::Clocks.systemClock", Kind: graph.EdgeProvides, FilePath: "repo/Clocks.java", Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingBean, "provides_for": "Clock"}},
		{From: "repo/plain.go::Plain", To: "repo/Clocks.java::Clocks", Kind: graph.EdgeCalls, FilePath: "repo/plain.go"},
		{From: "other/Other.java::Other.run", To: "repo/Clocks.java::Clocks", Kind: graph.EdgeConsumes, FilePath: "other/Other.java"},
	}
	if err := s.AddBatchChecked(nodes, edges); err != nil {
		t.Fatal(err)
	}
	seedEdges, err := s.ContractSeedEdgesContext(ctx, "repo")
	if err != nil || len(seedEdges) != 1 || seedEdges[0].Kind != graph.EdgeProvides || seedEdges[0].Meta[graph.MetaDIBinding] != graph.DIBindingBean {
		t.Fatalf("seed edges=%v err=%v", seedEdges, err)
	}
	seedNodes, err := s.ContractSeedNodesContext(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, node := range seedNodes {
		ids = append(ids, node.ID)
	}
	want := []string{"repo/Clocks.java::Clocks.systemClock", "repo/Settings.java::Settings.url"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("seed nodes=%v want %v", ids, want)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ContractSeedEdgesContext(canceled, "repo"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled seed edges=%v", err)
	}
	if _, err := s.ContractSeedNodesContext(canceled, "repo"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled seed nodes=%v", err)
	}
}

// TestContractReceiptWriteBracketHoldsAWriteInFlight pins the bracket order a
// scan relies on: begun and the path's seq move before the write executes,
// and done only after its transaction ended, committed or rolled back.
func TestContractReceiptWriteBracketHoldsAWriteInFlight(t *testing.T) {
	s := openCatalogStore(t)
	ctx := context.Background()
	row := boundaryStorageReceipt("held.go", "h1")
	began, committed, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	restore := SetContractReceiptWriteHookForTest(func(stage string, paths []string) {
		if len(paths) != 1 || paths[0] != row.FilePath {
			return
		}
		switch stage {
		case "begun":
			close(began)
		case "ending":
			close(committed)
			<-release
		}
	})
	defer restore()
	var wg sync.WaitGroup
	wg.Add(1)
	var writeErr error
	go func() {
		defer wg.Done()
		writeErr = s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{row})
	}()
	<-began
	<-committed
	if begun, done := s.ContractReceiptWriteSample("repo"); begun != 1 || done != 0 {
		t.Fatalf("a committed write before its done is in flight: begun=%d done=%d", begun, done)
	}
	if seqs := s.ContractReceiptWriteSeqs("repo", "", []string{row.FilePath}); seqs[row.FilePath] != 1 {
		t.Fatalf("the seq moved at begin: %v", seqs)
	}
	stamps, err := s.ContractBoundaryReceiptStampsContext(ctx, "repo", "", []string{row.FilePath})
	if err != nil || stamps[row.FilePath].Fingerprint != "h1" {
		t.Fatalf("the committed row is readable while the write is in flight: %v %v", stamps, err)
	}
	close(release)
	wg.Wait()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if begun, done := s.ContractReceiptWriteSample("repo"); begun != 1 || done != 1 {
		t.Fatalf("settled: begun=%d done=%d", begun, done)
	}
	restore()
	// A refused write still ends: its done is bumped after the rollback.
	bad := row
	bad.Fingerprint = ""
	if err := s.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{bad}); err == nil {
		t.Fatal("an incomplete receipt was accepted")
	}
	if begun, done := s.ContractReceiptWriteSample("repo"); begun != done {
		t.Fatalf("a refused write left the bracket open: begun=%d done=%d", begun, done)
	}
}
