package server

import (
	"testing"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/stretchr/testify/require"
)

// A timeout is reported with a code and no message, so rendering the message
// alone produced "invocation error: " and hid what had happened.
func TestDescribeError(t *testing.T) {
	cases := []struct {
		name     string
		err      *pb.Error
		expected string
	}{
		{"code and message", &pb.Error{Code: "unexpected", Message: "boom"}, "unexpected: boom"},
		{"code only", &pb.Error{Code: "timeout"}, "timeout"},
		{"message only", &pb.Error{Message: "boom"}, "boom"},
		{"neither", &pb.Error{}, "unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, describeError(tc.err))
		})
	}
}
