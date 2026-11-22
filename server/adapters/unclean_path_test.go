package adapters

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/dispatch"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ServeMux answers a path that is not clean with a 301 and a relative
// Location. The dispatcher cannot follow a relative redirect, so each such
// call failed. The path must go to the agent as it is, as snyk-broker does.
func TestBrokerDispatch_UncleanPathIsForwardedAsIs(t *testing.T) {
	cases := []struct {
		name, reqPath, agentPath string
	}{
		{"double slash", "/broker/tok/repos/o/r/contents//Jenkinsfile", "/repos/o/r/contents//Jenkinsfile"},
		{"dot segment", "/broker/tok/repos/o/r/contents/./Jenkinsfile", "/repos/o/r/contents/./Jenkinsfile"},
		{"query kept", "/broker/tok/a//b?ref=main", "/a//b?ref=main"},
		{"clean path", "/broker/tok/repos/o/r/contents/Jenkinsfile", "/repos/o/r/contents/Jenkinsfile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger := zap.NewNop()
			registry := tunnel.NewClientRegistry(logger)
			cfg := config.Config{DispatchTimeout: 5 * time.Second}
			m := metrics.New("test")
			h := NewHttpAdapter(cfg, registry, dispatch.NewDispatcher(cfg, registry, m, logger), m, logger)

			// Record the path the agent gets, then fail the send so the
			// call ends without a wait for a response.
			var agentPath string
			mustRegister(t, registry, broker.NewToken("tok"), tunnel.ClientIdentity{InstanceID: "agent-1"},
				&tunnel.StreamHandle{StreamID: "s1", Send: func(f *pb.ServerFrame) error {
					if start := f.GetCall().GetStart(); start != nil {
						agentPath = start.GetPseudoHeaders()[":path"]
					}
					return errors.New("stop after start")
				}})

			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.reqPath, nil))

			require.NotEqual(t, http.StatusMovedPermanently, w.Code)
			require.Empty(t, w.Header().Get("Location"))
			require.Equal(t, tc.agentPath, agentPath)
		})
	}
}

func TestIsCleanPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/broker/tok/a/b":   true,
		"/broker/tok/a/b/":  true,
		"/broker/tok/a//b":  false,
		"/broker/tok/./a":   false,
		"/broker/tok/a/../": false,
		"/broker/tok//":     false,
	} {
		require.Equal(t, want, isCleanPath(p), p)
	}
}
