//go:build performance

package indexer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestContractStuckRepairTakesAReleasedLaneWithinItsBudget: a repair that
// found the lane busy takes it within 200 ms of its release; the test
// allows twice that.
func TestContractStuckRepairTakesAReleasedLaneWithinItsBudget(t *testing.T) {
	require.LessOrEqual(t, busyLaneRepair(t), 2*200*time.Millisecond)
}
