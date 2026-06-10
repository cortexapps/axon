package cron

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCronFiresAddedJob verifies that a job added to the cron scheduler
// actually fires.  This exercises the real robfig/cron scheduler, which
// only runs jobs after the scheduler has been started.
func TestCronFiresAddedJob(t *testing.T) {
	c := New()

	var fired int32
	_, err := c.Add("@every 1s", func() {
		atomic.AddInt32(&fired, 1)
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&fired) > 0
	}, 3*time.Second, 50*time.Millisecond, "cron job never fired; scheduler was not started")
}
