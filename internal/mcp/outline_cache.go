package mcp

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const (
	outlineCacheEntries       = 32
	outlineCachePayloadBytes  = 64 << 10
	outlineCacheIdentityBytes = 4 << 10
)

type outlineResponseKey struct {
	reader            *store_sqlite.Store
	revision          uint64
	analysisReader    *store_sqlite.Store
	analysisRevision  uint64
	epoch, scopeEpoch uint64
	generation        int64
	ready             bool
	scope, mask       string
}

type outlineResponseCache struct {
	mu         sync.Mutex
	scopeEpoch uint64
	keys       []outlineResponseKey
	values     map[outlineResponseKey]map[string]any
}

// Only known concrete stores and the exact contract-core mask are cacheable.
// A composed/overlay reader may change without the base revision advancing.
// No capability probe tunnels through those readers to the physical store.
func (s *Server) outlineCacheKey(reader graph.Reader, scope graph.NodeScope, bound bool) (outlineResponseKey, bool) {
	if !bound {
		return outlineResponseKey{}, false
	}
	key := outlineResponseKey{}
	if wrapped, ok := reader.(contractCoreWrapped); ok {
		core := wrapped.contractCore()
		reader = core.Reader
		if core.contractIDs == nil {
			key.mask = "kinds"
		} else {
			ids := make([]string, 0, len(core.contractIDs))
			for id, excluded := range core.contractIDs {
				if excluded {
					ids = append(ids, id)
				}
			}
			slices.Sort(ids)
			data, _ := json.Marshal(ids)
			key.mask = "ids:" + string(data)
		}
	}
	store, ok := reader.(*store_sqlite.Store)
	if !ok {
		return outlineResponseKey{}, false
	}
	analysisStore, ok := s.graph.(*store_sqlite.Store)
	if !ok {
		return outlineResponseKey{}, false
	}
	// Both clocks are complete mutation clocks, not counts. The exact handle
	// fixes payload generation; rebinding another view changes the key too.
	key.reader, key.revision = store, store.MutationRevision()
	key.analysisReader, key.analysisRevision = analysisStore, analysisStore.MutationRevision()
	writer, _ := s.analysisGenerationBackends()
	if writer == nil || !s.analysisMu.TryRLock() {
		return outlineResponseKey{}, false
	}
	key.epoch = s.analysisEpoch
	key.generation = s.analysisGeneration.GenerationID
	key.ready = s.analysisGenerationReady
	current := s.analysisEpoch > 0 && s.communitiesToken.analysisRevision == writer.AnalysisMutationRevision() && s.communitiesToken.edgeIdentity == s.graph.EdgeIdentityRevisions()
	hotReady := s.hotspotsReady
	s.analysisMu.RUnlock()
	// Do not turn a transient failed/lazy materialization into a permanent
	// empty answer. Before the first analysis, absence retains legacy behavior.
	if current && !hotReady {
		return outlineResponseKey{}, false
	}
	repos := make([]string, 0, len(scope.RepoAllow))
	for repo, allowed := range scope.RepoAllow {
		if allowed {
			repos = append(repos, repo)
		}
	}
	slices.Sort(repos)
	data, _ := json.Marshal(struct {
		Workspace, Project string
		Narrow             bool
		Repos              []string
	}{scope.WorkspaceID, scope.ProjectID, len(scope.RepoAllow) > 0, repos})
	key.scope = string(data)
	if len(key.scope) > outlineCacheIdentityBytes || len(key.mask) > outlineCacheIdentityBytes {
		return outlineResponseKey{}, false
	}
	s.outlineResponses.mu.Lock()
	key.scopeEpoch = s.outlineResponses.scopeEpoch
	s.outlineResponses.mu.Unlock()
	return key, true
}

func (c *outlineResponseCache) clear() {
	c.mu.Lock()
	c.scopeEpoch++
	c.keys = nil
	c.values = nil
	c.mu.Unlock()
}
func (c *outlineResponseCache) get(key outlineResponseKey) (map[string]any, bool) {
	c.mu.Lock()
	payload, ok := c.values[key]
	c.mu.Unlock()
	if !ok {
		return nil, false
	}
	return cloneOutlinePayload(payload), true
}
func (c *outlineResponseCache) put(key outlineResponseKey, payload map[string]any) {
	data, err := json.Marshal(payload)
	if err != nil || len(data) > outlineCachePayloadBytes {
		return
	}
	owned := cloneOutlinePayload(payload)
	c.mu.Lock()
	defer c.mu.Unlock()
	if key.scopeEpoch != c.scopeEpoch {
		return
	}
	// Keep only the currently observed graph/analysis epoch, never a revision
	// history. Scope variants share this fixed-size FIFO for the current proof.
	if len(c.keys) > 0 {
		last := c.keys[len(c.keys)-1]
		if last.reader != key.reader || last.revision != key.revision || last.analysisReader != key.analysisReader || last.analysisRevision != key.analysisRevision || last.epoch != key.epoch || last.generation != key.generation || last.ready != key.ready {
			c.keys = nil
			c.values = nil
		}
	}
	if _, ok := c.values[key]; ok {
		return
	}
	if len(c.keys) == outlineCacheEntries {
		delete(c.values, c.keys[0])
		copy(c.keys, c.keys[1:])
		c.keys = c.keys[:len(c.keys)-1]
	}
	if c.values == nil {
		c.values = make(map[outlineResponseKey]map[string]any)
	}
	c.keys = append(c.keys, key)
	c.values[key] = owned
}

// The payload has one small, fixed map/list vocabulary. Copy it before
// formatting/degradation and own every retained string: backend substrings or
// request decorations must not retain large buffers or mutate another reply.
func cloneOutlinePayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		out[key] = cloneOutlineValue(value)
	}
	return out
}
func cloneOutlineValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return cloneOutlinePayload(v)
	case []map[string]any:
		if v == nil {
			return v
		}
		out := make([]map[string]any, len(v))
		for i, item := range v {
			out[i] = cloneOutlinePayload(item)
		}
		return out
	case []outlineLanguage:
		if v == nil {
			return v
		}
		out := slices.Clone(v)
		for i := range out {
			out[i].Name = strings.Clone(out[i].Name)
		}
		return out
	case string:
		return strings.Clone(v)
	default:
		return v // The remaining payload leaves are immutable scalar numbers.
	}
}
