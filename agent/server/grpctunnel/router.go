package grpctunnel

import (
	"errors"
	"net/http"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"go.uber.org/zap"
)

// RouteError carries an HTTP status hint for CallCancel when a call cannot
// be routed or executed.
type RouteError struct {
	Code   int32
	Reason string
}

func (e *RouteError) Error() string { return e.Reason }

// Router adapts tunnel CallStart frames onto the shared accept-file
// router. All routing semantics (rule matching, origin/pool resolution,
// target URL construction, header/auth injection) live in
// acceptfile.Router so every relay transport shares one implementation;
// this type only extracts the pseudo-headers and maps errors to CallCancel
// status hints.
type Router struct {
	inner *acceptfile.Router
}

// NewRouter creates a Router over rendered accept file rules.
func NewRouter(rules []acceptfile.AcceptFileRuleWrapper, logger *zap.Logger) *Router {
	return &Router{inner: acceptfile.NewRouter(rules, logger)}
}

// Route resolves a CallStart to a BackendRequest, or a *RouteError with an
// HTTP status hint.
func (rt *Router) Route(start *pb.CallStart) (*BackendRequest, error) {
	method := start.PseudoHeaders[":method"]
	rawPath := start.PseudoHeaders[":path"]

	routed, err := rt.inner.Route(method, rawPath, start.Headers)
	if err != nil {
		var invalid *acceptfile.InvalidRequestError
		switch {
		case errors.Is(err, acceptfile.ErrNoRoute):
			return nil, &RouteError{Code: http.StatusNotFound, Reason: err.Error()}
		case errors.As(err, &invalid):
			return nil, &RouteError{Code: http.StatusBadRequest, Reason: invalid.Reason}
		default:
			return nil, &RouteError{Code: http.StatusBadGateway, Reason: err.Error()}
		}
	}

	return &BackendRequest{
		Method: routed.Method,
		URL:    routed.URL,
		Header: routed.Header,
		Kind:   start.Kind,
	}, nil
}
