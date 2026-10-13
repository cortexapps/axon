package handler

import (
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

var testPolicy = LivenessPolicy{TimeoutThreshold: 3, MinFailureAge: 3 * time.Minute}

// newTestLiveness returns a tracker whose clock the test advances by hand.
func newTestLiveness(t *testing.T, policy LivenessPolicy) (*Liveness, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	l := NewLiveness(zap.NewNop(), policy, nil)
	l.now = func() time.Time { return clock }
	return l, &clock
}

func TestLiveness_Healthy(t *testing.T) {
	timeouts := func(l *Liveness, n int) {
		for i := 0; i < n; i++ {
			l.RecordTimeout("handler1")
		}
	}

	cases := []struct {
		name     string
		policy   LivenessPolicy
		arrange  func(l *Liveness, clock *time.Time)
		healthy  bool
		contains string
	}{
		{
			name:   "disabled threshold zero",
			policy: LivenessPolicy{TimeoutThreshold: 0, MinFailureAge: 0},
			arrange: func(l *Liveness, clock *time.Time) {
				l.RecordStreamConnected()
				timeouts(l, 10)
				*clock = clock.Add(time.Hour)
			},
			healthy:  true,
			contains: "disabled",
		},
		{
			name:   "never connected",
			policy: testPolicy,
			arrange: func(l *Liveness, clock *time.Time) {
				timeouts(l, 10)
				*clock = clock.Add(time.Hour)
			},
			healthy:  true,
			contains: "no dispatch stream",
		},
		{
			name:   "below threshold",
			policy: testPolicy,
			arrange: func(l *Liveness, clock *time.Time) {
				l.RecordStreamConnected()
				timeouts(l, 2)
				*clock = clock.Add(time.Hour)
			},
			healthy: true,
		},
		{
			name:   "streak too young",
			policy: testPolicy,
			arrange: func(l *Liveness, clock *time.Time) {
				l.RecordStreamConnected()
				timeouts(l, 3)
				*clock = clock.Add(2 * time.Minute)
			},
			healthy: true,
		},
		{
			name:   "streak old enough",
			policy: testPolicy,
			arrange: func(l *Liveness, clock *time.Time) {
				l.RecordStreamConnected()
				timeouts(l, 3)
				*clock = clock.Add(3 * time.Minute)
			},
			healthy:  false,
			contains: "3 consecutive",
		},
		{
			name:   "any SDK report resets the streak",
			policy: testPolicy,
			arrange: func(l *Liveness, clock *time.Time) {
				l.RecordStreamConnected()
				timeouts(l, 3)
				*clock = clock.Add(time.Hour)
				l.RecordSuccess()
				// A new streak is measured from its own first timeout.
				timeouts(l, 3)
				*clock = clock.Add(time.Minute)
			},
			healthy: true,
		},
		{
			name:   "no age gate when zero",
			policy: LivenessPolicy{TimeoutThreshold: 3, MinFailureAge: 0},
			arrange: func(l *Liveness, clock *time.Time) {
				l.RecordStreamConnected()
				timeouts(l, 3)
			},
			healthy: false,
		},
		{
			name:   "reconnect does not reset",
			policy: testPolicy,
			arrange: func(l *Liveness, clock *time.Time) {
				l.RecordStreamConnected()
				timeouts(l, 3)
				l.RecordStreamDisconnected()
				l.RecordStreamConnected()
				*clock = clock.Add(time.Hour)
			},
			healthy: false,
		},
		{
			name:   "disconnect does not change health",
			policy: testPolicy,
			arrange: func(l *Liveness, clock *time.Time) {
				l.RecordStreamConnected()
				timeouts(l, 1)
				l.RecordStreamDisconnected()
				*clock = clock.Add(time.Hour)
			},
			healthy: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, clock := newTestLiveness(t, tc.policy)
			tc.arrange(l, clock)
			healthy, reason := l.Healthy(*clock)
			require.Equal(t, tc.healthy, healthy, "reason: %s", reason)
			if tc.contains != "" {
				require.Contains(t, reason, tc.contains)
			}
		})
	}
}

func TestLiveness_NilReceiverIsHealthy(t *testing.T) {
	var l *Liveness
	l.RecordStreamConnected()
	l.RecordTimeout("handler1")
	l.RecordSuccess()
	l.RecordStreamDisconnected()
	healthy, reason := l.Healthy(time.Now())
	require.True(t, healthy)
	require.Empty(t, reason)
	require.Equal(t, LivenessSnapshot{}, l.Snapshot())
}

func TestLiveness_SnapshotReportsState(t *testing.T) {
	l, clock := newTestLiveness(t, testPolicy)
	l.RecordStreamConnected()
	l.RecordTimeout("handler1")
	l.RecordTimeout("handler2")

	snap := l.Snapshot()
	require.Equal(t, "handler2", snap.LastTimeoutHandler)
	require.True(t, snap.Enabled)
	require.True(t, snap.EverConnected)
	require.Equal(t, 1, snap.StreamsConnected)
	require.Equal(t, 2, snap.ConsecutiveTimeouts)
	require.EqualValues(t, 2, snap.TotalTimeouts)
	require.NotNil(t, snap.StreakStartedAt)
	require.Equal(t, *clock, *snap.StreakStartedAt)
	require.Nil(t, snap.LastReportAt)
}

// Transitions are logged once each way and mirrored in the gauge, including
// the transition that only time, observed by a probe, can cause.
func TestLiveness_TransitionsLogOnceAndSetGauge(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	clock := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	registry := prometheus.NewRegistry()
	l := NewLiveness(zap.New(core), testPolicy, registry)
	l.now = func() time.Time { return clock }

	l.RecordStreamConnected()
	for i := 0; i < 3; i++ {
		l.RecordTimeout("slow_batch")
	}
	require.Equal(t, float64(1), testutil.ToFloat64(l.gauge), "streak too young: still healthy")
	require.Empty(t, logs.FilterMessage("handler liveness unhealthy").All())

	clock = clock.Add(3 * time.Minute)
	healthy, _ := l.Healthy(clock)
	require.False(t, healthy)
	l.Healthy(clock)
	l.Healthy(clock)
	require.Equal(t, float64(0), testutil.ToFloat64(l.gauge))
	require.Len(t, logs.FilterMessage("handler liveness unhealthy").All(), 1, "one warn per transition")
	unhealthy := logs.FilterMessage("handler liveness unhealthy").All()[0]
	require.Equal(t, zapcore.WarnLevel, unhealthy.Level)
	require.Equal(t, "slow_batch", unhealthy.ContextMap()["last_timeout_handler"],
		"an operator needs the handler name to tell a misconfigured timeout from a wedged pool")

	l.RecordSuccess()
	require.Equal(t, float64(1), testutil.ToFloat64(l.gauge))
	require.Len(t, logs.FilterMessage("handler liveness recovered").All(), 1)
}

func TestLiveness_ConcurrentRecordsAreRaceFree(t *testing.T) {
	l, _ := newTestLiveness(t, testPolicy)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				switch (i + j) % 4 {
				case 0:
					l.RecordStreamConnected()
				case 1:
					l.RecordTimeout("h")
				case 2:
					l.RecordSuccess()
				case 3:
					l.Healthy(time.Now())
				}
			}
		}(i)
	}
	wg.Wait()
	l.Snapshot()
}
