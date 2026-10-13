package handler

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// ErrInvocationTimeout marks an agent-side timeout: the SDK was sent the
// invocation and never reported before its deadline. The server's timer path
// wraps it so the manager can tell it from an error the SDK reported itself;
// both SDKs report code "timeout" for their own deadlines, and a report of
// any kind proves the SDK's dispatch loop is alive.
var ErrInvocationTimeout = errors.New("invocation timed out")

// LivenessPolicy says when a run of agent-side timeouts means the SDK has
// stopped draining invocations.
type LivenessPolicy struct {
	// TimeoutThreshold is how many consecutive invocations must end in an
	// agent-side timeout, with no SDK report in between, before the handler
	// loop is reported unhealthy. 0 disables the guard.
	TimeoutThreshold int
	// MinFailureAge is how long that streak must have persisted before it is
	// reported, so a short burst of slow upstream does not restart a pod that
	// is still draining. 0 reports as soon as the threshold is met.
	MinFailureAge time.Duration
}

// LivenessSnapshot is the tracker's state as served from the healthcheck.
type LivenessSnapshot struct {
	Enabled             bool       `json:"enabled"`
	StreamsConnected    int        `json:"streams_connected"`
	EverConnected       bool       `json:"ever_connected"`
	ConsecutiveTimeouts int        `json:"consecutive_timeouts"`
	TotalTimeouts       uint64     `json:"total_timeouts"`
	StreakStartedAt     *time.Time `json:"streak_started_at,omitempty"`
	LastReportAt        *time.Time `json:"last_report_at,omitempty"`
}

// Liveness decides whether the SDK handler loop is still draining work.
//
// The incident it exists for: every SDK worker blocked in a hung API call,
// the SDK stopped reading the dispatch stream, every invocation the agent
// sent timed out, and the healthcheck kept answering 200 because it checked
// nothing. The agent and the SDK share a container, so a failing probe is
// what restarts the SDK.
//
// All methods are safe on a nil receiver and behave as "disabled", so code
// paths and tests without a tracker need no wiring.
type Liveness struct {
	policy LivenessPolicy
	logger *zap.Logger
	now    func() time.Time
	gauge  prometheus.Gauge

	mu                  sync.Mutex
	streams             int
	everConnected       bool
	consecutiveTimeouts int
	totalTimeouts       uint64
	streakStartedAt     time.Time
	lastReportAt        time.Time
	unhealthy           bool // last published state, for transition logging
}

func NewLiveness(logger *zap.Logger, policy LivenessPolicy, registry *prometheus.Registry) *Liveness {
	l := &Liveness{
		policy: policy,
		logger: logger.Named("liveness"),
		now:    time.Now,
		gauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "axon_handler_liveness",
			Help: "1 while the SDK handler loop is draining invocations, 0 once the healthcheck reports it stalled. Updated on each invocation outcome and each probe.",
		}),
	}
	l.gauge.Set(1)
	if registry != nil {
		registry.MustRegister(l.gauge)
	}
	return l
}

// RecordStreamConnected notes that an SDK opened a dispatch stream. It does
// not reset a timeout streak: a reconnecting SDK whose worker pool is still
// wedged proves nothing until it reports.
func (l *Liveness) RecordStreamConnected() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.streams++
	l.everConnected = true
	l.evaluateLocked(l.now())
}

func (l *Liveness) RecordStreamDisconnected() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.streams > 0 {
		l.streams--
	}
	l.evaluateLocked(l.now())
}

// RecordTimeout notes an invocation that ended in an agent-side timeout:
// expired waiting in the dispatch queue, or sent and never reported.
func (l *Liveness) RecordTimeout() {
	if l == nil {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.consecutiveTimeouts == 0 {
		l.streakStartedAt = now
	}
	l.consecutiveTimeouts++
	l.totalTimeouts++
	l.evaluateLocked(now)
}

// RecordSuccess notes any SDK-originated report, result or error, on time or
// late. Each one proves the SDK's dispatch loop is reading and reporting, so
// it ends the current timeout streak.
func (l *Liveness) RecordSuccess() {
	if l == nil {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.consecutiveTimeouts = 0
	l.streakStartedAt = time.Time{}
	l.lastReportAt = now
	l.evaluateLocked(now)
}

// Healthy reports the current state and, when unhealthy, why. It evaluates
// rather than reads: the age gate can flip the state with no event, and the
// probe is what observes that.
func (l *Liveness) Healthy(now time.Time) (bool, string) {
	if l == nil {
		return true, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.evaluateLocked(now)
}

func (l *Liveness) Snapshot() LivenessSnapshot {
	if l == nil {
		return LivenessSnapshot{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	snap := LivenessSnapshot{
		Enabled:             l.policy.TimeoutThreshold > 0,
		StreamsConnected:    l.streams,
		EverConnected:       l.everConnected,
		ConsecutiveTimeouts: l.consecutiveTimeouts,
		TotalTimeouts:       l.totalTimeouts,
	}
	if !l.streakStartedAt.IsZero() {
		t := l.streakStartedAt
		snap.StreakStartedAt = &t
	}
	if !l.lastReportAt.IsZero() {
		t := l.lastReportAt
		snap.LastReportAt = &t
	}
	return snap
}

func (l *Liveness) Policy() LivenessPolicy {
	if l == nil {
		return LivenessPolicy{}
	}
	return l.policy
}

// evaluateLocked computes the state and publishes a transition once, to the
// gauge and the log. Caller holds mu.
func (l *Liveness) evaluateLocked(now time.Time) (bool, string) {
	healthy, reason := l.stateLocked(now)
	if healthy == !l.unhealthy {
		return healthy, reason
	}
	l.unhealthy = !healthy
	if healthy {
		l.gauge.Set(1)
		l.logger.Warn("handler liveness recovered",
			zap.Time("last_report_at", l.lastReportAt))
	} else {
		l.gauge.Set(0)
		l.logger.Warn("handler liveness unhealthy",
			zap.Int("consecutive_timeouts", l.consecutiveTimeouts),
			zap.Duration("streak_age", now.Sub(l.streakStartedAt)),
			zap.Time("last_report_at", l.lastReportAt),
			zap.String("reason", reason))
	}
	return healthy, reason
}

func (l *Liveness) stateLocked(now time.Time) (bool, string) {
	if l.policy.TimeoutThreshold <= 0 {
		return true, "liveness guard disabled"
	}
	if !l.everConnected {
		return true, "no dispatch stream has connected yet"
	}
	if l.consecutiveTimeouts < l.policy.TimeoutThreshold {
		return true, ""
	}
	age := now.Sub(l.streakStartedAt)
	if age < l.policy.MinFailureAge {
		return true, ""
	}
	last := "never"
	if !l.lastReportAt.IsZero() {
		last = l.lastReportAt.Format(time.RFC3339)
	}
	return false, fmt.Sprintf("%d consecutive invocation timeouts over %s with no SDK report (last report: %s)",
		l.consecutiveTimeouts, age.Truncate(time.Second), last)
}
