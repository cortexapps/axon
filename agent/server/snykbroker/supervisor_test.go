package snykbroker

import (
	"bytes"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
	output := bytes.Buffer{}
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
	output := bytes.Buffer{}
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

	supervisor := NewSupervisor(
		"bash",
		[]string{"-c", "exit 1"},
		map[string]string{
			"BROKER_TOKEN":      "test_token",
			"BROKER_SERVER_URL": "http://example.com",
		},
		time.Millisecond*50,
	)
	output := bytes.Buffer{}
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
	output := bytes.Buffer{}
	supervisor.output = &output
	supervisor.panicOnMaxRetries = false // this disables the panic and just returns the error
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
