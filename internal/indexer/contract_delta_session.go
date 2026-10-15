package indexer

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

// A session owns the actor's values exclusively while its LRU reservation
// remains charged. Fresh evidence, postpasses and seeds belong only to the job.
type contractDeltaSession struct {
	cache                                                        *contractDeltaCache
	identity                                                     contractDeltaIdentity
	knownRaw                                                     map[*contractUnit]int64
	disabled                                                     bool
	disabledPath, disabledReason, disabledMetaPath, disabledType string
	membershipUnknown                                            bool
	receiptStamps                                                map[string]ContractReceiptStamp
	receipts                                                     map[string]contractDeltaReceipt
}

func newContractDeltaSession(p *contractDeltaCache, snap ContractFollowupSnapshot, cfg config.IndexConfig) (*contractDeltaSession, error) {
	repos := make([]string, 0, len(snap.Inputs))
	for _, input := range snap.Inputs {
		repos = append(repos, input.State.RepoPrefix)
	}
	slices.Sort(repos)
	repos = slices.Compact(repos)
	encoded, err := json.Marshal(struct {
		Config  config.IndexConfig
		Repos   map[string]config.IndexConfig
		Options any
	}{cfg, snap.RepoConfigs, snap.RepoExtractionOptions})
	if err != nil {
		return nil, err
	}
	modules, err := json.Marshal(snap.TrackedRepoModules)
	if err != nil {
		return nil, err
	}
	return &contractDeltaSession{cache: p, identity: contractDeltaIdentity{selection: snap.Key.CheckoutID + "\x00" + strings.Join(repos, "\x00"), config: contractInputHash(encoded), modules: contractInputHash(modules), receiptVersion: contractBoundaryReceiptVersion}, knownRaw: make(map[*contractUnit]int64)}, nil
}

// Receipt payloads are parsed only for changed fingerprints; immutable facts
// survive physical folds. Full stamp/census scans and core-read proofs still run.
func (s *contractDeltaSession) readReceipts(c *contractConvergence, stamps map[string]ContractReceiptStamp) (map[string]contractDeltaReceipt, error) {
	if s.receiptStamps == nil {
		s.receiptStamps = make(map[string]ContractReceiptStamp)
		s.receipts = make(map[string]contractDeltaReceipt)
		if s.cache != nil {
			for path, r := range s.cache.receipts {
				s.receipts[path] = r
				s.receiptStamps[path] = s.cache.clock.last[path]
			}
		}
	}
	out := make(map[string]contractDeltaReceipt, len(stamps))
	missing := make(map[string]ContractReceiptStamp)
	for path, stamp := range stamps {
		r, had := s.receipts[path]
		if had && s.receiptStamps[path].Fingerprint == stamp.Fingerprint {
			out[path] = r
		} else {
			missing[path] = stamp
		}
	}
	if len(missing) > 0 {
		if c.snap.deltaReceipts != nil {
			read, err := c.snap.deltaReceipts(c.ctx, missing)
			if err != nil {
				return nil, err
			}
			for path, r := range read {
				out[path] = r
			}
		} else if c.snap.Scan == nil {
			// Frozen worker fixtures bind bytes directly to their synthetic stamp.
			for path, stamp := range missing {
				out[path] = contractDeltaReceipt{boundary: stamp.Fingerprint}
			}
		}
	}
	s.receipts, s.receiptStamps = out, stamps
	return out, nil
}

func (s *contractDeltaSession) adopt(c *contractConvergence) {
	p := s.cache
	if p == nil || p.identity != s.identity || p.deltas+1 >= contractDeltaRefresh {
		return
	}
	c.units, c.live, c.excluded, c.suspect = p.units, p.live, p.excluded, p.suspect
	c.retryOwners, c.declarers = p.retryOwners, p.declarers
	c.clock, c.contentAt, c.maxContent, c.contents = p.clock, p.contentAt, p.maxContent, p.contents
	for path, unit := range p.units {
		if size, ok := p.rawBytes[path]; ok {
			s.knownRaw[unit] = size
		} else {
			s.ownRaw(unit)
		}
	}
	// Ownership transfer, not restoration into a reset clock. No second reader
	// can access these maps while the scheduler holds this actor's job slot.
}

func (s *contractDeltaSession) ownRaw(unit *contractUnit) {
	if _, known := s.knownRaw[unit]; known || s.disabled {
		return
	}
	for _, raw := range unit.raw {
		if _, err := json.Marshal(raw.Meta); err != nil {
			s.disabled, s.disabledPath, s.disabledReason, s.disabledType = true, unit.path, "invalid_metadata_json", fmt.Sprintf("%T", err)
			var unsupported *json.UnsupportedTypeError
			if errors.As(err, &unsupported) {
				s.disabledType = unsupported.Type.String()
			}
			return
		}
		if !contractDeltaMetaValue(reflect.ValueOf(raw.Meta)) {
			s.disabled, s.disabledPath, s.disabledReason = true, unit.path, "unsupported_metadata"
			s.disabledMetaPath, s.disabledType = contractDeltaUnsupportedMeta(reflect.ValueOf(raw.Meta), "meta")
			return
		}
	}
	unit.raw = cloneContractDeltaRaw(unit.raw)
	s.knownRaw[unit] = contractRetainedSize(reflect.ValueOf(unit.raw))
}

// finish retains proven partial state after cancellation/key movement too;
// complete is granted only by the convergence fence, never by worker success.
// Raw values are normalized/charged once per newly extracted unit. Proof maps
// still copy/normalize linearly here; their cost remains a profiling gate.
func (s *contractDeltaSession) finish(c *contractConvergence, complete bool) {
	if s.disabled {
		c.logger.Info("contract cache checkpoint", zap.String("reason", s.disabledReason), zap.String("file", s.disabledPath), zap.String("metadata_path", s.disabledMetaPath), zap.String("metadata_type", s.disabledType), zap.Bool("cache_present", false))
		s.cache = nil
		return
	}
	receipts, err := s.readReceipts(c, c.clock.last)
	if err != nil {
		receipts = make(map[string]contractDeltaReceipt)
	}
	p := &contractDeltaCache{identity: s.identity, units: cloneContractUnitsMode(c.units, false), live: cloneContractDeltaMap(c.live), excluded: cloneContractDeltaMap(c.excluded), suspect: cloneContractDeltaMap(c.suspect), retryOwners: cloneContractIndex(c.retryOwners), declarers: cloneContractIndex(c.declarers), clock: cloneContractClock(c.clock), contentAt: cloneContractDeltaMap(c.contentAt), maxContent: c.maxContent, contents: cloneContractDeltaMap(c.contents), receipts: cloneContractDeltaReceipts(receipts), complete: complete, rawBytes: make(map[string]int64, len(c.units))}
	if s.cache != nil && c.report.Mode == viewmetrics.ContractModeDelta {
		p.deltas = s.cache.deltas + 1
	}
	if !complete && s.cache != nil {
		p.deltas = s.cache.deltas
	}
	p.indexReaders()
	// Charge proof structures without traversing unchanged raw metadata again.
	proofUnits := make(map[string]*contractUnit, len(p.units))
	for path, unit := range p.units {
		v := *unit
		v.raw = nil
		proofUnits[path] = &v
		p.rawBytes[path] = s.knownRaw[c.units[path]]
	}
	withoutUnits := *p
	withoutUnits.units = nil
	p.retainedBytes = contractRetainedSize(reflect.ValueOf(&withoutUnits)) + contractRetainedSize(reflect.ValueOf(proofUnits))
	proofBytes := p.retainedBytes
	rawBytes := int64(0)
	for _, size := range p.rawBytes {
		p.retainedBytes += size
		rawBytes += size
	}
	c.logger.Info("contract cache checkpoint", zap.String("reason", "prepared"), zap.Bool("cache_present", true), zap.Bool("complete", p.complete), zap.Int("units", len(p.units)), zap.Int64("retained_bytes", p.retainedBytes), zap.Int64("raw_bytes", rawBytes), zap.Int64("proof_bytes", proofBytes), zap.Int("receipts", len(p.receipts)), zap.Int("deltas", p.deltas))
	s.cache = p
}

func contractDeltaMembershipUnknown(work []graph.ContractWork) bool {
	for _, row := range work {
		if row.FilePath == row.RepoPrefix+"/" {
			return true
		}
		for _, cause := range row.Scope.Causes {
			if cause == "dependency_membership_unknown" {
				return true
			}
		}
	}
	return false
}

// Called only after shape rejection; reports field/type, never metadata values.
func contractDeltaUnsupportedMeta(v reflect.Value, path string) (string, string) {
	if !v.IsValid() {
		return "", ""
	}
	switch v.Kind() {
	case reflect.Interface:
		if !v.IsNil() {
			return contractDeltaUnsupportedMeta(v.Elem(), path)
		}
		return "", ""
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return path, v.Type().String()
		}
		iter := v.MapRange()
		for iter.Next() {
			if p, kind := contractDeltaUnsupportedMeta(iter.Value(), path+"."+iter.Key().String()); p != "" {
				return p, kind
			}
		}
		return "", ""
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if p, kind := contractDeltaUnsupportedMeta(v.Index(i), path+"[]"); p != "" {
				return p, kind
			}
		}
		return "", ""
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return "", ""
	}
	return path, v.Type().String()
}
