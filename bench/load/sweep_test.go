package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSweepCensusPacingAndOutsideScopeInventory(t *testing.T) {
	plan, err := readingPlan(sampleCorpus())
	if err != nil {
		t.Fatal(err)
	}
	var advertised []any
	domains := make(map[string][]string)
	for _, r := range plan {
		domains[r.Tool] = append(domains[r.Tool], r.Operation)
	}
	for domain, ops := range domains {
		key := "operation"
		if domain == "analyze" {
			key = "kind"
		}
		advertised = append(advertised, map[string]any{"name": domain, "inputSchema": map[string]any{"properties": map[string]any{key: map[string]any{"enum": ops}}}})
	}
	advertised = append(advertised, map[string]any{"name": "publish_review", "inputSchema": map[string]any{"properties": map[string]any{"operation": map[string]any{"enum": []string{"post"}}}}})
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Method == "tools/list" {
			return map[string]any{"tools": advertised}, nil
		}
		return stubContent(map[string]any{"ok": true}), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	requests, inventory, err := discoverSweep(ctx, client, sampleCorpus())
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 21 || len(inventory) != 22 {
		t.Fatalf("sweep census selected=%d inventory=%d", len(requests), len(inventory))
	}
	var mu sync.Mutex
	var waits []time.Duration
	phase := Phase{}
	err = runSweep(ctx, runOptions{Profile: "agent", Seed: 42, Deadline: time.Second}, client, privateRepo{Prefix: "sample"}, requests, inventory, &phase, func(_ context.Context, delay time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, delay)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, wait := range waits {
		if wait < 2*time.Second || wait > 5*time.Second {
			t.Fatalf("sweep think=%s", wait)
		}
	}
	groups := make(map[int]int)
	analysisSteps := make(map[string]int)
	seen := make(map[string]bool)
	for _, a := range phase.Attempts {
		groups[a.Step]++
		if seen[a.Tool] {
			t.Fatalf("duplicate benchmark operation %s", a.Tool)
		}
		seen[a.Tool] = true
		if a.Tool == "analyze.hotspots" || a.Tool == "analyze.dead_code" {
			analysisSteps[a.Tool] = a.Step
		}
	}
	if len(seen) != 21 || len(groups) != len(waits) {
		t.Fatalf("sweep omissions: %d operations, %v groups/%v waits", len(seen), groups, waits)
	}
	for step, count := range groups {
		if count < 3 || count > 5 {
			t.Fatalf("step%d burst=%d", step, count)
		}
	}
	if analysisSteps["analyze.hotspots"] == analysisSteps["analyze.dead_code"] {
		t.Fatal("mutually exclusive analysis operations share a sweep burst")
	}
	for _, entry := range inventory {
		if entry.Tool == "publish_review" {
			if entry.Status != "outside_scope" {
				t.Fatal(entry)
			}
		} else if entry.Status != "measured" {
			t.Fatalf("benchmark operation not measured: %+v", entry)
		}
	}
}

func TestSweepBurstPartitionNeverLeavesOneOrTwo(t *testing.T) {
	for count := 3; count < 100; count++ {
		for seed := range 30 {
			pace := newPacing(agentProfile, uint64(seed))
			remaining := count
			for remaining > 0 {
				size := sweepBurst(remaining, pace)
				if size < 3 || size > 5 || size > remaining {
					t.Fatalf("count=%d seed=%d remaining=%d burst=%d", count, seed, remaining, size)
				}
				remaining -= size
			}
		}
	}
}
