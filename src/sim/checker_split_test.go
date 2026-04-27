package sim

import (
	"testing"

	"github.com/antflydb/antfly/src/store"
	storedb "github.com/antflydb/antfly/src/store/db"
	"github.com/stretchr/testify/require"
)

func TestCheckerIsSplitActive_IgnoresPhaseNone(t *testing.T) {
	checker := NewChecker(CheckerConfig{})

	status := &store.ShardStatus{
		State: store.ShardState_Default,
		ShardInfo: store.ShardInfo{
			SplitState: &storedb.SplitState{},
		},
	}

	require.False(t, checker.isSplitActive(status))

	status.SplitState.SetPhase(storedb.SplitState_PHASE_SPLITTING)
	require.True(t, checker.isSplitActive(status))
}
