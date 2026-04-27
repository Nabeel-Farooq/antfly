package sim

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTraceRecorder_CompactTraceRetainsRequestedKinds(t *testing.T) {
	recorder := NewTraceRecorder()
	base := time.Unix(1_700_000_000, 0).UTC()

	recorder.RecordEvent(base, "split", "shard=4000 state=Splitting")
	recorder.RecordEvent(base.Add(time.Second), "action", "step=0 action=write")
	recorder.RecordEvent(base.Add(2*time.Second), "action", "step=1 action=tick")
	recorder.RecordEvent(base.Add(3*time.Second), "action", "step=2 action=check")

	trace := recorder.CompactTraceRetainKinds(2, 0, "split")

	require.Contains(t, trace, "[split]")
	require.Contains(t, trace, "step=1 action=tick")
	require.Contains(t, trace, "step=2 action=check")
	require.NotContains(t, trace, "step=0 action=write")
}
