package indexer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func runCachedContractFixture(t *testing.T, f *contractJobFixture, p **contractDeltaCache) ContractFollowupReport {
	t.Helper()
	f.layers++
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: p, epoch: f.epoch.Load, wait: f.waitPublished})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.NotNil(t, *p)
	require.True(t, (*p).complete)
	return report
}

func TestContractDeltaWorkerRouteReplayEqualsIndependentCold(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	var p *contractDeltaCache
	first := runCachedContractFixture(t, f, &p)
	require.Equal(t, viewmetrics.ContractModeFill, first.Mode)
	require.Equal(t, len(files), first.Files)
	previousClock := p.clock.clock
	unchanged := p.units["fixture/helper.go"].raw
	heldRaw := p.units["fixture/routes.go"].raw
	beforeRaw := cloneContractRecords(heldRaw)
	for i, path := range []string{"/second", "/third"} {
		files["routes.go"] = strings.ReplaceAll(jobRoutes, "RoutePath", `"`+path+`"`)
		f.save("routes.go", files["routes.go"])
		report := runCachedContractFixture(t, f, &p)
		require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
		require.Equal(t, ContractFollowupChangedBoundary, report.RebuildReason)
		require.Equal(t, []string{"fixture/routes.go"}, report.ChangedFiles)
		require.Equal(t, 1, report.Files)
		require.Greater(t, p.clock.clock, previousClock)
		require.Equal(t, unchanged, p.units["fixture/helper.go"].raw)
		require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
		require.Equal(t, beforeRaw, heldRaw)
		require.Equal(t, i+1, p.deltas)
		previousClock = p.clock.clock
	}
	// A postpass must never mutate the original raw unit across later jobs.
	require.NotEmpty(t, beforeRaw)
	require.Equal(t, beforeRaw, heldRaw)
}

func TestContractDeltaWorkerProducerLifecycleEqualsIndependentCold(t *testing.T) {
	for _, row := range []struct {
		name, initial, edited string
		remove                bool
	}{
		{name: "value", initial: jobConsts, edited: strings.ReplaceAll(jobConsts, "/before", "/after")},
		{name: "rename", initial: jobConsts, edited: strings.ReplaceAll(jobConsts, "RoutePath", "RenamedPath")},
		{name: "negative_becomes_answered", initial: "package fixture\nconst OtherPath = \"/other\"\n", edited: jobConsts},
		{name: "remove", initial: jobConsts, remove: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			files := map[string]string{"routes.go": jobRoutes, "consts.go": row.initial, "helper.go": jobHelper}
			f := newContractJobFixture(t, files)
			var p *contractDeltaCache
			runCachedContractFixture(t, f, &p)
			oldClock := p.clock.clock
			if row.remove {
				f.remove("consts.go")
				delete(files, "consts.go")
			} else {
				f.save("consts.go", row.edited)
				files["consts.go"] = row.edited
			}
			report := runCachedContractFixture(t, f, &p)
			require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
			require.Greater(t, p.clock.clock, oldClock)
			require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
			require.Less(t, report.Files, len(files)+1)
			if !row.remove {
				require.Contains(t, report.ChangedFiles, "fixture/consts.go")
			}
		})
	}
}
