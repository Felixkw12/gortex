package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPublicEditAcknowledgementAndIndexedVisibility(t *testing.T) {
	var mu sync.Mutex
	var marker string
	editCalls := 0
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		mu.Lock()
		defer mu.Unlock()
		args, _ := frame.Params["arguments"].(map[string]any)
		if frame.Params["name"] == "edit" {
			editCalls++
			if args["operation"] != "file" || args["match"] != "package sample\n" {
				return nil, &rpcError{Code: -32602, Message: "invalid edit"}
			}
			marker = "GortexLoadMarkerA1S2"
			if !strings.Contains(args["replacement"].(string), marker) {
				return nil, &rpcError{Code: -32602, Message: "missing marker"}
			}
			return stubContent(map[string]any{"mutation_id": "receipt", "status": "applied"}), nil
		}
		return stubContent(map[string]any{"results": []any{map[string]any{"name": marker, "id": "sample/sample.go::" + marker}}}), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	c := sampleCorpus()
	c.EditAnchor = "package sample\n"
	c.EditTemplate = "\nfunc {{marker}}() {}\n"
	observation, rows := editAndObserve(ctx, runOptions{Deadline: time.Second}, client, privateRepo{Corpus: c, Prefix: "sample"}, 1, 2)
	if observation.Outcome != "visible" || observation.VisibilityMS.Value == nil || len(observation.Acknowledgement) == 0 || len(rows) != 2 || rows[0].Tool != "edit.file" || rows[1].Role != "probe" {
		t.Fatalf("edit/readback: %+v %+v", observation, rows)
	}
	mu.Lock()
	defer mu.Unlock()
	if editCalls != 1 {
		t.Fatalf("public edit calls=%d", editCalls)
	}
}

func TestVisibilityRequiresActualIndexedIdentity(t *testing.T) {
	for _, data := range []string{`{"query":"Marker"}`, `{"source":"func Marker() {}"}`, `{"name":"Other"}`, `{"name":"Marker"}`, `{"id":"ext::Marker","name":"Marker"}`} {
		if containsIdentity(json.RawMessage(data), "Marker") {
			t.Fatalf("non-indexed identity passed: %s", data)
		}
	}
	if !containsIdentity(json.RawMessage(`{"results":[{"id":"sample/a.go::Marker","name":"Marker"}]}`), "Marker") {
		t.Fatal("indexed result rejected")
	}
}

func TestRunOptionsFloorAndExplicitSaturation(t *testing.T) {
	base := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir()}
	options, err := parseRun(base)
	if err != nil || options.Profile != "agent" {
		t.Fatalf("defaults: %+v %v", options, err)
	}
	if _, err := parseRun(append(base, "--profile", "saturation")); err != nil {
		t.Fatal(err)
	}
	if _, err := parseRun(append(base, "--host", "floor", "--scenario", "read")); err != nil {
		t.Fatalf("floor should count active readers only: %v", err)
	}
	for _, extra := range [][]string{{"--host", "floor", "--scenario", "edit"}, {"--host", "floor", "--scenario", "many"}, {"--host", "floor", "--scenario", "worktrees"}, {"--host", "reference", "--keep-state"}, {"--scenario", "unknown"}} {
		if _, err := parseRun(append(base, extra...)); err == nil {
			t.Fatalf("invalid options accepted: %v", extra)
		}
	}
}

func TestStateCleanupPreservesEvidence(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"store.db", "store.db-wal", "store.db-shm", "daemon.log", "read.cpu.pprof", "config.yaml"} {
		if err := os.WriteFile(root+string(os.PathSeparator)+name, []byte("evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := privateRun{Root: root}
	if err := p.removeState(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"store.db", "store.db-wal", "store.db-shm"} {
		if _, err := os.Stat(root + string(os.PathSeparator) + name); !os.IsNotExist(err) {
			t.Fatalf("state retained %s: %v", name, err)
		}
	}
	for _, name := range []string{"daemon.log", "read.cpu.pprof", "config.yaml"} {
		if _, err := os.Stat(root + string(os.PathSeparator) + name); err != nil {
			t.Fatalf("evidence removed %s: %v", name, err)
		}
	}
}
