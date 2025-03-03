package util

import (
	"fmt"
	"strconv"
	"time"
)

var timeFormats = []string{
	time.RFC3339,
	time.RFC1123Z,
	time.RFC822Z,
}

const (
	secondsThreshold      = int64(1e10) // 10^10
	millisecondsThreshold = int64(1e13) // 10^13
)

func TimeFromString(timeStr string) (time.Time, error) {

	for _, format := range timeFormats {
		t, err := time.Parse(format, timeStr)
		if err == nil {
			return t, nil
		}
	}

	// check int formats
	intVal, err := strconv.ParseInt(timeStr, 10, 64)
	if err == nil {
		// we have an int, check if its seconds, milliseconds or nanoseconds
		if intVal < secondsThreshold {
			return time.Unix(intVal, 0), nil
		} else if intVal < millisecondsThreshold {
			return time.Unix(0, intVal*int64(time.Millisecond)), nil
		}
	}

	return time.Time{}, fmt.Errorf("failed to parse time: %s", timeStr)

}

func TimeToString(t time.Time) string {
	return t.Format(time.RFC3339)
}
