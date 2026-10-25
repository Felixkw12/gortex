package mcp

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/zzet/gortex/internal/analysis"
)

func TestReleaseTransientAnalysisDropsPublishedCompatibilityViews(t *testing.T) {
	s := &Server{
		analysisGenerationReady: true,
		communities:             &analysis.CommunityResult{},
		processes:               &analysis.ProcessResult{},
		hotspots:                []analysis.HotspotEntry{{ID: "hot"}},
		hotspotsReady:           true,
	}

	if !s.releaseTransientAnalysisIfIdle() {
		t.Fatal("expected published transient analysis to be released")
	}
	if s.communities != nil || s.processes != nil || len(s.hotspots) != 1 || !s.hotspotsReady {
		t.Fatalf("transient analysis retained: communities=%v processes=%v hotspots=%v ready=%v",
			s.communities != nil, s.processes != nil, s.hotspots != nil, s.hotspotsReady)
	}
}

func TestReleaseTransientAnalysisDropsOversizeHotspots(t *testing.T) {
	for _, entries := range [][]analysis.HotspotEntry{
		{{ID: strings.Repeat("x", hotspotResultRetentionBytes)}},
		make([]analysis.HotspotEntry, 1, hotspotResultRetentionBytes/int(unsafe.Sizeof(analysis.HotspotEntry{}))+1),
	} {
		s := &Server{analysisGenerationReady: true, hotspots: entries, hotspotsReady: true}
		if hotspotResultRetainable(entries) {
			t.Fatal("oversize result accepted")
		}
		s.releaseTransientAnalysisIfIdle()
		if s.hotspots != nil || s.hotspotsReady {
			t.Fatal("oversize result retained")
		}
	}
}

func TestHotspotResultRetentionIncludesStringsAndSpareCapacity(t *testing.T) {
	entryBytes := int(unsafe.Sizeof(analysis.HotspotEntry{}))
	entries := []analysis.HotspotEntry{{ID: strings.Repeat("x", hotspotResultRetentionBytes-entryBytes)}}
	if !hotspotResultRetainable(entries) {
		t.Fatal("exact budget rejected")
	}
	entries[0].FilePath = "x"
	if hotspotResultRetainable(entries) {
		t.Fatal("string bytes outside the budget retained")
	}
	backing := []analysis.HotspotEntry{{ID: "visible"}, {Name: strings.Repeat("x", hotspotResultRetentionBytes)}}
	if hotspotResultRetainable(backing[:1]) {
		t.Fatal("strings in the unused backing slice were not accounted for")
	}
}

func TestReleaseTransientAnalysisKeepsUnpublishedCompatibilityViews(t *testing.T) {
	communities := &analysis.CommunityResult{}
	s := &Server{communities: communities}

	if s.releaseTransientAnalysisIfIdle() {
		t.Fatal("unpublished transient analysis must remain available")
	}
	if s.communities != communities {
		t.Fatal("unpublished compatibility view was dropped")
	}
}
