package snykbroker

import (
	"bytes"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// syncBuffer is a bytes.Buffer safe to read while the supervisor's line pumps
// are still writing. Those pumps outlive Wait() — it returns when the run loop
// gives up, not when the last line has been drained — so an unguarded buffer
// races with any assertion on its contents.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestStart_SuccessExit(t *testing.T) {

	supervisor := NewSupervisor(
		"bash",
		[]string{"-c", "echo \"$BROKER_TOKEN $BROKER_SERVER_URL\""},
		map[string]string{
			"BROKER_TOKEN":      "test_token",
			"BROKER_SERVER_URL": "http://example.com",
		},
		time.Millisecond*10,
	)
	output := syncBuffer{}
	supervisor.output = &output

	err := supervisor.Start(1, 1)
	require.NoError(t, err)
}

func TestStart_Restart(t *testing.T) {

	supervisor := NewSupervisor(
		"bash",
		[]string{"-c", "sleep 300"},
		map[string]string{
			"BROKER_TOKEN":      "test_token",
			"BROKER_SERVER_URL": "http://example.com",
		},
		time.Millisecond*10,
	)
	output := syncBuffer{}
	supervisor.output = &output

	err := supervisor.Start(1, 1)
	require.NoError(t, err)
	require.NotEqual(t, 0, supervisor.Pid())
	err = supervisor.Close()
	require.NoError(t, err)
	require.Equal(t, 0, supervisor.Pid())

	err = supervisor.Start(1, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already started")
}

func TestStart_FastFail(t *testing.T) {

	// test that if the command fails quickly off the bat, we don't retry anymore

	// NewSupervisor doubles this, so 50ms gave the process 100ms to spawn,
	// run and exit before it stopped counting as an immediate failure — which
	// a loaded CI runner can exceed just forking bash, failing the test for
	// reasons unrelated to what it checks. A process meant to run
	// indefinitely that dies inside a second is still immediate by any
	// reading, so the wider budget costs the assertion nothing.
	supervisor := NewSupervisor(
		"bash",
		[]string{"-c", "exit 1"},
		map[string]string{
			"BROKER_TOKEN":      "test_token",
			"BROKER_SERVER_URL": "http://example.com",
		},
		time.Millisecond*500,
	)
	output := syncBuffer{}
	supervisor.output = &output

	err := supervisor.Start(2, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed immediately")
}

func TestStart_MaxRetries(t *testing.T) {

	// Test that the command will be re run unless it hits the max retries
	// in the window

	supervisor := NewSupervisor(
		"bash",
		[]string{"-c", "echo run;sleep .05; exit 1"},
		map[string]string{
			"BROKER_TOKEN":      "test_token",
			"BROKER_SERVER_URL": "http://example.com",
		},
		time.Second,
	)
	output := syncBuffer{}
	supervisor.output = &output
	supervisor.panicOnMaxRetries = false
	supervisor.fastFailTime = 1 * time.Millisecond

	err := supervisor.Start(2, time.Second)
	require.NoError(t, err)
	err = supervisor.Wait()
	require.Error(t, err)
	require.Contains(t, err.Error(), "max retries")
	println(output.String())
	runRegEx := regexp.MustCompile("run")
	require.GreaterOrEqual(t, len(runRegEx.FindAllString(output.String(), -1)), 3)
}
