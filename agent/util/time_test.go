package util

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTimeFromString_RFC3339(t *testing.T) {
	timeStr := "2023-10-10T10:10:10Z"
	expectedTime, _ := time.Parse(time.RFC3339, timeStr)

	parsedTime, err := TimeFromString(timeStr)
	require.NoError(t, err)
	require.Equal(t, expectedTime, parsedTime)
}

func TestTimeFromString_RFC1123Z(t *testing.T) {
	timeStr := "Tue, 10 Oct 2023 10:10:10 +0000"
	expectedTime, _ := time.Parse(time.RFC1123Z, timeStr)

	parsedTime, err := TimeFromString(timeStr)
	require.NoError(t, err)
	require.Equal(t, expectedTime, parsedTime)
}

func TestTimeFromString_RFC822Z(t *testing.T) {
	timeStr := "10 Oct 23 10:10 +0000"
	expectedTime, _ := time.Parse(time.RFC822Z, timeStr)

	parsedTime, err := TimeFromString(timeStr)
	require.NoError(t, err)
	require.Equal(t, expectedTime, parsedTime)
}

func TestTimeFromString_Seconds(t *testing.T) {
	timeStr := "1634567890"
	expectedTime := time.Unix(1634567890, 0)

	parsedTime, err := TimeFromString(timeStr)
	require.NoError(t, err)
	require.Equal(t, expectedTime, parsedTime)
}

func TestTimeFromString_Milliseconds(t *testing.T) {
	timeStr := "1634567890123"
	expectedTime := time.Unix(0, 1634567890123*int64(time.Millisecond))

	parsedTime, err := TimeFromString(timeStr)
	require.NoError(t, err)
	require.Equal(t, expectedTime, parsedTime)
}

func TestTimeFromString_InvalidFormat(t *testing.T) {
	timeStr := "invalid-time-format"

	_, err := TimeFromString(timeStr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to parse time")
}
