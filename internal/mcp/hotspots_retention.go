package mcp

import (
	"unsafe"

	"github.com/zzet/gortex/internal/analysis"
)

const hotspotResultRetentionBytes = 1 << 20

// hotspotResultRetainable bounds the entire backing slice plus the strings
// its entries keep alive. Oversize results are returned unchanged to their
// caller, then released; a top-N prefix would change scoped ranking semantics.
func hotspotResultRetainable(entries []analysis.HotspotEntry) bool {
	entryBytes := int(unsafe.Sizeof(analysis.HotspotEntry{}))
	remaining := hotspotResultRetentionBytes - int(unsafe.Sizeof(entries))
	if cap(entries) > remaining/entryBytes {
		return false
	}
	remaining -= cap(entries) * entryBytes
	for _, entry := range entries[:cap(entries)] {
		for _, value := range []string{entry.ID, entry.Name, entry.Kind, entry.FilePath} {
			if len(value) > remaining {
				return false
			}
			remaining -= len(value)
		}
	}
	return true
}
