package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestPairedWireRequestsRotateWithinEachCategory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := newPairedCoordinator(ctx, runOptions{Deadline: time.Second}, []string{"tag", "main"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.cancel()
	var mu sync.Mutex
	var received []string
	wireHashes := make(map[string][]string)
	for i, label := range c.labels {
		socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
			mu.Lock()
			defer mu.Unlock()
			received = append(received, label)
			hash, _ := hashJSON(frame.Params)
			wireHashes[label] = append(wireHashes[label], hash)
			return stubContent(map[string]any{"ok": true}), nil
		})
		client, err := openMCP(ctx, socket, "/different/private/"+label)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		c.states[i] = &pairedState{p: &privateRun{Repos: []privateRepo{{Corpus: sampleCorpus(), Prefix: "sample"}}}, clients: []*socketClient{client}, phase: &Phase{}}
	}
	plan, err := readingPlan(sampleCorpus())
	if err != nil {
		t.Fatal(err)
	}
	c.callAcross(ctx, plan[0], 0, 0, 0, 0, "workload", time.Second, -1)
	c.callAcross(ctx, plan[0], 0, 0, 1, 0, "workload", time.Second, -1)
	c.callAcross(ctx, plan[3], 0, 0, 2, 0, "workload", time.Second, -1)
	c.callAcross(ctx, plan[0], 0, 0, 3, 0, "probe", time.Second, -1)
	c.callAcross(ctx, plan[0], 0, 0, 3, 0, "probe", time.Second, -1)
	want := []string{"tag", "main", "main", "tag", "tag", "main", "tag", "main", "main", "tag"}
	mu.Lock()
	got := append([]string(nil), received...)
	hashesEqual := reflect.DeepEqual(wireHashes["tag"], wireHashes["main"])
	mu.Unlock()
	if !reflect.DeepEqual(got, want) || !hashesEqual {
		t.Fatalf("requests/order differ: %v, equal normalized wire=%t", got, hashesEqual)
	}
	if len(c.records) != 5 {
		t.Fatalf("logical request census=%d", len(c.records))
	}
	if c.records[3].TupleHash == c.records[4].TupleHash {
		t.Fatal("repeated visibility probes reused logical tuple identity")
	}
	for _, record := range c.records {
		hash, _ := hashJSON(record.Tuple)
		if hash != record.TupleHash {
			t.Fatal("tuple hash mismatch")
		}
		for _, sample := range record.Samples {
			if sample.RequestHash != record.Tuple.RequestHash || sample.Repo != record.Tuple.Repo || sample.Commit != record.Tuple.Commit {
				t.Fatal("build received different normalized tuple")
			}
		}
	}
	ratios := matchedLatencyRatios(c.labels, c.records)["main/tag"]
	if ratios["workload.search.symbols"].Matched != 2 || ratios["probe.search.symbols"].Matched != 2 {
		t.Fatalf("probe/workload matching mixed: %+v", ratios)
	}
}

func TestPairedFailureDoesNotEraseOtherBuildSample(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := newPairedCoordinator(ctx, runOptions{}, []string{"tag", "main"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.cancel()
	for i, label := range c.labels {
		socket := startStub(t, func(stubFrame) (any, *rpcError) {
			if label == "main" {
				return nil, &rpcError{Code: -32000, Message: "retained failure"}
			}
			return stubContent(map[string]any{"ok": true}), nil
		})
		client, err := openMCP(ctx, socket, "")
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		c.states[i] = &pairedState{p: &privateRun{Repos: []privateRepo{{Corpus: sampleCorpus(), Prefix: "sample"}}}, clients: []*socketClient{client}, phase: &Phase{}}
	}
	plan, _ := readingPlan(sampleCorpus())
	c.callAcross(ctx, plan[0], 0, 0, 0, 0, "workload", time.Second, -1)
	row := c.records[0]
	if row.Samples["tag"].Outcome != "success" || row.Samples["main"].Outcome != "error" || row.Samples["main"].Error == "" {
		t.Fatalf("failure attribution lost: %+v", row)
	}
	ratio := matchedLatencyRatios(c.labels, c.records)["main/tag"]["workload.search.symbols"]
	if ratio.Matched != 0 || ratio.Excluded != 1 || ratio.Median.Value != nil {
		t.Fatalf("failed pair became successful ratio: %+v", ratio)
	}
}

func TestPairedPreBarrierFailureAndCancellationCleanOwnedDaemon(t *testing.T) {
	for _, mode := range []string{"failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("LOAD_DAEMON_FIXTURE", "serve")
			corpus := gitFixture(t)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			parent, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			options := runOptions{Binary: binary, Profile: "agent", Host: "native", Agents: 1, Duration: time.Second, Deadline: time.Second, WarmTimeout: 10 * time.Second, StopTimeout: 5 * time.Second}
			coordinator, err := newPairedCoordinator(parent, options, []string{"tag", "main"}, "")
			if err != nil {
				t.Fatal(err)
			}
			defer coordinator.cancel()
			type facts struct{ root, socket string }
			owned := make(chan facts, 1)
			options.pairedDriver = func(ctx context.Context, p *privateRun, clients, sweepClients []*socketClient, phase *Phase) error {
				owned <- facts{p.Root, p.Socket}
				return coordinator.driver(0)(ctx, p, clients, sweepClients, phase)
			}
			done := make(chan error, 1)
			go func() {
				_, _, _, _, _, err := runPhase(coordinator.ctx, options, "read", []Corpus{corpus}, "", t.TempDir())
				done <- err
			}()
			select {
			case <-coordinator.firstReady:
			case <-parent.Done():
				t.Fatal("first owned daemon did not reach paired barrier")
			}
			paths := <-owned
			failure := errors.New("other build failed before readiness")
			if mode == "failure" {
				coordinator.abort(failure)
			} else {
				coordinator.cancel()
			}
			select {
			case err := <-done:
				if err == nil || (mode == "failure" && !errors.Is(err, failure)) {
					t.Fatalf("pre-barrier outcome: %v", err)
				}
			case <-parent.Done():
				t.Fatal("pre-barrier cancellation leaked owned daemon")
			}
			if _, err := os.Stat(filepath.Dir(paths.socket)); !os.IsNotExist(err) {
				t.Fatalf("owned socket directory remains: %v", err)
			}
			if _, err := os.Stat(filepath.Join(paths.root, "repos")); !os.IsNotExist(err) {
				t.Fatalf("owned clone state remains: %v", err)
			}
			if len(coordinator.records) != 0 {
				t.Fatal("failed readiness barrier produced successful request records")
			}
		})
	}
}
