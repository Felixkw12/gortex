package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
)

// Sweep every advertised domain/operation explicitly. Unsupported prerequisites
// are evidence, not successful measurements or a claim of complete coverage.
func discoverSweep(ctx context.Context, client *socketClient, corpus Corpus) ([]request, []SweepEntry, error) {
	known, err := readingPlan(corpus)
	if err != nil {
		return nil, nil, err
	}
	builders := make(map[string]request)
	for _, r := range known {
		builders[r.name()] = r
	}
	var selected []request
	var inventory []SweepEntry
	seen := make(map[string]bool)
	cursor := ""
	for pages := 0; pages < 100; pages++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		data, err := client.call(ctx, "tools/list", params)
		if err != nil {
			return nil, inventory, err
		}
		var result struct {
			Tools []struct {
				Name   string `json:"name"`
				Schema struct {
					Properties map[string]struct {
						Enum []string `json:"enum"`
					} `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
			Next string `json:"nextCursor"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, inventory, err
		}
		for _, tool := range result.Tools {
			ops := tool.Schema.Properties["operation"].Enum
			if tool.Name == "analyze" {
				ops = tool.Schema.Properties["kind"].Enum
			}
			if len(ops) == 0 {
				ops = []string{""}
			}
			for _, op := range ops {
				r := request{Tool: tool.Name, Operation: op}
				if seen[r.name()] {
					return nil, inventory, errors.New("duplicate advertised sweep operation")
				}
				seen[r.name()] = true
				entry := SweepEntry{Tool: r.Tool, Operation: op, Status: "outside_scope", Reason: "outside the documented 21-operation benchmark contract"}
				if build, ok := builders[r.name()]; ok {
					selected = append(selected, build)
					entry.Status, entry.Reason = "planned", ""
				}
				inventory = append(inventory, entry)
			}
		}
		if result.Next == "" {
			break
		}
		if result.Next == cursor || pages == 99 {
			return nil, inventory, errors.New("invalid tool inventory pagination")
		}
		cursor = result.Next
	}
	for _, r := range known {
		if !seen[r.name()] {
			inventory = append(inventory, SweepEntry{Tool: r.Tool, Operation: r.Operation, Status: "missing", Reason: "planned operation not advertised by this binary"})
		}
	}
	slices.SortFunc(selected, func(a, b request) int {
		if a.name() < b.name() {
			return -1
		}
		if a.name() > b.name() {
			return 1
		}
		return 0
	})
	return selected, inventory, nil
}
