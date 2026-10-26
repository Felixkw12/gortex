package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type profile string

const (
	agentProfile      profile = "agent"
	saturationProfile profile = "saturation"
)

func parseProfile(value string) (profile, error) {
	p := profile(value)
	if p != agentProfile && p != saturationProfile {
		return "", fmt.Errorf("unknown profile %q (want agent or saturation)", value)
	}
	return p, nil
}

type pacing struct {
	profile profile
	random  *rand.Rand
}

func newPacing(p profile, seed uint64) *pacing {
	return &pacing{profile: p, random: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

func (p *pacing) think() time.Duration {
	if p.profile == saturationProfile {
		return 0
	}
	return 2*time.Second + time.Duration(p.random.Int64N(int64(3*time.Second)+1))
}

func (p *pacing) burst() int { return 3 + p.random.IntN(3) }

func busyBackoff(retry int) time.Duration {
	if retry >= 5 {
		return time.Second
	}
	return 50 * time.Millisecond * time.Duration(1<<max(0, retry))
}

type request struct {
	Tool      string         `json:"tool"`
	Operation string         `json:"operation"`
	Arguments map[string]any `json:"arguments"`
}

// Workload is the complete normalized offered load. Binary paths, private
// clone paths and revisions belong to the run identity, never the plan hash.
type Workload struct {
	Scenario        string    `json:"scenario"`
	Agents          int       `json:"agents"`
	EditAgents      int       `json:"edit_agents"`
	LinkedWorktrees int       `json:"linked_worktrees"`
	DurationSec     float64   `json:"duration_seconds"`
	DeadlineSec     float64   `json:"product_deadline_seconds"`
	Seed            int64     `json:"seed"`
	Profile         string    `json:"profile"`
	Operations      []request `json:"operations"`
}

func (r request) name() string {
	if r.Operation == "" {
		return r.Tool
	}
	return r.Tool + "." + r.Operation
}

func hashJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// These are logical, repo-relative targets. Expansion to a private clone's
// absolute path happens only at dispatch; the plan hash is identical per build.
func readingPlan(c Corpus) ([]request, error) {
	if err := validateCorpus(c); err != nil {
		return nil, err
	}
	file := map[string]any{"file": c.File}
	symbol := map[string]any{"symbol": c.Symbol}
	return []request{
		{Tool: "search", Operation: "symbols", Arguments: map[string]any{"operation": "symbols", "query": c.Query}},
		{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": symbol}},
		{Tool: "read", Operation: "file", Arguments: map[string]any{"operation": "file", "target": file}},
		{Tool: "relations", Operation: "dependents", Arguments: map[string]any{"operation": "dependents", "target": file}},
		{Tool: "change", Operation: "verify", Arguments: map[string]any{"operation": "verify", "target": symbol}},
	}, nil
}

var commitPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
var labelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

func validateCorpus(c Corpus) error {
	if !labelPattern.MatchString(c.Label) || c.Source == "" || !commitPattern.MatchString(c.Commit) {
		return errors.New("corpus requires a neutral label, source, and full 40-digit commit SHA")
	}
	path := filepath.FromSlash(c.File)
	if c.File == "" || filepath.IsAbs(path) || filepath.Clean(path) == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator)) || strings.Contains(c.File, "\\") {
		return fmt.Errorf("corpus %s: file must be a repo-relative path", c.Label)
	}
	if c.Symbol == "" || strings.HasPrefix(c.Symbol, "ext::") || c.Query == "" {
		return fmt.Errorf("corpus %s: requires a source-backed symbol and query; ext:: targets are excluded", c.Label)
	}
	return nil
}
