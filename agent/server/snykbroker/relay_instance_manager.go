package snykbroker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/cortexapps/axon/util"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const defaultSnykBroker = "snyk-broker"
const brokerPort = 7343

// restartRequest is sent to the restart channel by any code path that
// needs to restart the broker.  The generation field ties the request
// to the broker lifecycle that triggered it, so the consumer can
// discard stale requests that arrive after a restart has already
// happened.
type restartRequest struct {
	reason     string
	generation int32
}

type RelayInstanceManager interface {
	Start() error
	Restart() error
	Close() error
}

type relayInstanceManager struct {
	integrationInfo   common.IntegrationInfo
	registration      Registration
	config            config.AgentConfig
	logger            *zap.Logger
	supervisor        *Supervisor
	running           atomic.Bool
	startCount        atomic.Int32
	generation        atomic.Int32 // incremented on each Start(), used to deduplicate restart requests
	tokenInfo         *tokenInfo
	operationsCounter *prometheus.CounterVec
	transport         *http.Transport

	reflector *RegistrationReflector
	restartCh chan restartRequest
}

type tokenInfo struct {
	ServerUri   string
	OriginalUri string
	Token       string
	HasChanged  bool
}

func (t *tokenInfo) equals(other *tokenInfo) bool {
	if other == nil {
		return false
	}
	if t.OriginalUri != other.OriginalUri {
		return false
	}
	if t.Token != other.Token {
		return false
	}
	return true
}

type RelayInstanceManagerParams struct {
	fx.In
	Lifecycle       fx.Lifecycle `optional:"true"`
	Config          config.AgentConfig
	Logger          *zap.Logger
	IntegrationInfo common.IntegrationInfo
	HttpServer      cortexHttp.Server
	Registration    Registration
	Transport       *http.Transport        `optional:"true"`
	Registry        *prometheus.Registry   `optional:"true"`
	Reflector       *RegistrationReflector `optional:"true"`
}

func NewRelayInstanceManager(
	p RelayInstanceManagerParams,
) RelayInstanceManager {
	mgr := &relayInstanceManager{
		config:          p.Config,
		logger:          p.Logger,
		integrationInfo: p.IntegrationInfo,
		registration:    p.Registration,
		operationsCounter: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "broker_operations",
				Help: "Counter for broker operations",
			},
			[]string{"integration", "alias", "operation", "status"},
		),
		transport: p.Transport,
		restartCh: make(chan restartRequest, 1),
	}

	p.HttpServer.RegisterHandler(mgr)

	if p.Registry != nil {
		p.Registry.MustRegister(mgr.operationsCounter)
	}

	mgr.reflector = p.Reflector
	go mgr.restartConsumer()

	if p.Lifecycle != nil {
		p.Lifecycle.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				return mgr.Start()
			},
			OnStop: func(ctx context.Context) error {
				return mgr.Close()
			},
		})
	}
	return mgr
}

func (r *relayInstanceManager) RegisterRoutes(mux *mux.Router) error {
	subRouter := mux.PathPrefix(fmt.Sprintf("%s/broker", cortexHttp.AxonPathRoot)).Subrouter()
	subRouter.HandleFunc("/restart", r.handleRestart)
	subRouter.HandleFunc("/reregister", r.handleReregister)
	subRouter.HandleFunc("/systemcheck", r.handleSystemCheck)
	return nil
}

func (r *relayInstanceManager) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// should never be called, here just to satisfy the interface
	w.WriteHeader(http.StatusNotFound)
}

func (r *relayInstanceManager) emitOperationCounter(operation string, success bool) {
	status := "success"
	if !success {
		status = "failure"
	}
	r.operationsCounter.WithLabelValues(r.integrationInfo.Integration.String(), r.integrationInfo.Alias, operation, status).Inc()
}

func (r *relayInstanceManager) handleRestart(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	err := r.Restart()
	if err != nil {
		r.logger.Error("Unable to reregister", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Unable to reregister"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *relayInstanceManager) handleReregister(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	r.logger.Info("Reregistering broker")
	info, err := r.getUrlAndToken()
	r.emitOperationCounter("reregister", err == nil)
	if err != nil {
		r.logger.Error("Unable to reregister", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Unable to reregister"))
		return
	}

	if info.HasChanged {
		r.Restart()
	}
}

func (r *relayInstanceManager) getSnykBrokerPort() int {
	if r.config.SnykBrokerPort == 0 {
		return brokerPort
	}
	return r.config.SnykBrokerPort
}

func (r *relayInstanceManager) handleSystemCheck(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/systemcheck", r.getSnykBrokerPort()))
	if err != nil {
		r.logger.Error("Unable to get system check", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Unable to get system check"))
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header().Set(k, strings.Join(v, ","))
	}
	w.WriteHeader(resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		r.logger.Error("Unable to read system check response", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(body)

}

// requestRestart sends a restart request for the given generation.
// If the channel is full (a restart is already pending) the request
// is dropped — the pending restart will cover it.
func (r *relayInstanceManager) requestRestart(reason string, gen int32) {
	select {
	case r.restartCh <- restartRequest{reason: reason, generation: gen}:
		r.logger.Info("Restart requested", zap.String("reason", reason), zap.Int32("generation", gen))
	default:
		r.logger.Debug("Restart already pending, dropping duplicate",
			zap.String("reason", reason), zap.Int32("generation", gen))
	}
}

// restartConsumer is the single goroutine that processes restart
// requests.  It deduplicates by generation: if the broker has already
// moved to a newer generation by the time we read the request, the
// request is stale and we skip it.
//
// It also doubles as the idle watchdog: every minute it checks
// shouldRestart() and produces a restart request if the broker has
// been idle too long.
func (r *relayInstanceManager) restartConsumer() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		var req restartRequest
		select {
		case req = <-r.restartCh:
			// explicit restart request
		case <-ticker.C:
			if ok, reason := r.shouldRestart(); ok {
				req = restartRequest{reason: reason, generation: r.generation.Load()}
			} else {
				continue
			}
		}

		current := r.generation.Load()
		if req.generation != current {
			r.logger.Info("Ignoring stale restart request",
				zap.String("reason", req.reason),
				zap.Int32("requestGeneration", req.generation),
				zap.Int32("currentGeneration", current))
			continue
		}
		r.logger.Info("Processing restart request",
			zap.String("reason", req.reason),
			zap.Int32("generation", req.generation))
		r.emitOperationCounter("restart_"+req.reason, true)

		// Retry with backoff until the restart succeeds or we're
		// shut down.  This is the persistent outer watchdog: no
		// matter how many times the broker or registration fails,
		// we keep trying.
		for attempt := 0; ; attempt++ {
			if err := r.Restart(); err != nil {
				delay := r.restartBackoff(attempt)
				r.logger.Error("Restart failed, will retry",
					zap.String("reason", req.reason),
					zap.Int("attempt", attempt+1),
					zap.Duration("backoff", delay),
					zap.Error(err))
				time.Sleep(delay)
				// If we were shut down while sleeping, stop.
				if !r.running.Load() {
					return
				}
				continue
			}
			break
		}
	}
}

// shouldRestart checks whether the broker should be restarted due to
// idle timeout.  Returns true and the reason string if a restart is needed.
func (r *relayInstanceManager) shouldRestart() (bool, string) {
	if r.config.RelayIdleTimeout == 0 || r.reflector == nil {
		return false, ""
	}
	if !r.config.HttpRelayReflectorMode.ReflectsTraffic() {
		return false, ""
	}
	if time.Since(r.reflector.LastTrafficTime()) >= r.config.RelayIdleTimeout {
		return true, "idle_timeout"
	}
	return false, ""
}

// restartBackoff returns an exponential backoff duration capped at 60 seconds.
func (r *relayInstanceManager) restartBackoff(attempt int) time.Duration {
	base := 5 * time.Second
	d := base << uint(attempt) // 5s, 10s, 20s, 40s, 60s ...
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

var errSkipBroker = errors.New("NoBrokerToken")

func (r *relayInstanceManager) Restart() error {

	var err error

	defer func() {
		r.emitOperationCounter("restart", err == nil)
	}()

	r.logger.Info("Restarting broker, shutting down existing broker")
	// re-register and restart supervisor
	err = r.Close()
	if err != nil {
		r.logger.Error("unable to close supervisor on Restart", zap.Error(err))
	}

	// Wait for the broker port to become available.
	// After the process exits, TCP sockets may remain in TIME_WAIT state,
	// causing EADDRINUSE if we start too quickly.
	port := r.getSnykBrokerPort()
	if waitErr := r.waitForPortAvailable(port, 10*time.Second); waitErr != nil {
		r.logger.Warn("Port not available after timeout, proceeding anyway",
			zap.Int("port", port), zap.Error(waitErr))
	}

	r.logger.Info("Restarting broker")
	err = r.Start()
	if err != nil {
		return fmt.Errorf("unable to start supervisor on Restart: %w", err)
	}
	return nil
}

// waitForPortAvailable waits until the given port is available for binding,
// or until the timeout expires. This handles the TCP TIME_WAIT issue where
// the OS hasn't released the port yet after the previous process exited.
func (r *relayInstanceManager) waitForPortAvailable(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			r.logger.Debug("Port is available", zap.Int("port", port))
			return nil
		}
		r.logger.Debug("Port not yet available, waiting",
			zap.Int("port", port), zap.Error(err))
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("port %d not available after %v", port, timeout)
}

func (r *relayInstanceManager) getUrlAndToken() (*tokenInfo, error) {
	uri, token, err := r.getUrlAndTokenCore()
	if err != nil {
		return nil, err
	}

	tokenInfo := &tokenInfo{
		ServerUri:   uri,
		OriginalUri: uri,
		Token:       token,
	}

	if !tokenInfo.equals(r.tokenInfo) {
		r.logger.Info("Registration info has changed", zap.String("uri", tokenInfo.ServerUri), zap.String("token", tokenInfo.Token))
		tokenInfo.HasChanged = true
		r.tokenInfo = tokenInfo
	}

	// Route BROKER_SERVER_URL through reflector when mode reflects registration (registration, all).
	if r.reflector != nil && r.config.HttpRelayReflectorMode.ReflectsRegistration() {
		tokenInfo.ServerUri = r.reflector.ProxyURI(tokenInfo.ServerUri, WithDefault(true))
	}

	return tokenInfo, nil

}

func (r *relayInstanceManager) getUrlAndTokenCore() (string, string, error) {

	serverUri := os.Getenv("BROKER_SERVER_URL")
	token := os.Getenv("BROKER_TOKEN")
	if serverUri != "" && token != "" {
		return serverUri, token, nil
	}

	if r.config.DryRun {
		r.logger.Info("Not starting broker due to DRYRUN and missing token")
		return "", "", errSkipBroker
	}

	reg, err := r.registration.Register(r.integrationInfo.Integration, r.integrationInfo.Alias)
	r.emitOperationCounter("register", err == nil)
	if err != nil {
		return "", "", err
	}

	if serverUri == "" {
		serverUri = reg.ServerUri
	}

	return serverUri, reg.Token, nil
}

func (r *relayInstanceManager) getAcceptFilePath() string {
	return path.Join(os.TempDir(), fmt.Sprintf("axon-accept-file.%s.%s.%v.json", r.integrationInfo.Integration, r.integrationInfo.Alias, os.Getpid()))
}

func (r *relayInstanceManager) Start() error {

	if !r.running.CompareAndSwap(false, true) {
		return fmt.Errorf("already started")
	}

	r.generation.Add(1)

	executable := defaultSnykBroker

	if directPath := os.Getenv("SNYK_BROKER_PATH"); directPath != "" {
		executable = directPath
	}

	af, err := r.integrationInfo.ToAcceptFile(r.config, r.logger)
	if err != nil {
		r.logger.Error("Error creating accept file", zap.Error(err))
		return fmt.Errorf("error creating accept file: %w", err)
	}

	rendered, err := af.Render(r.logger, func(renderContext acceptfile.RenderContext) error {
		// Check if any routes have custom headers - these require traffic reflection mode
		for _, route := range renderContext.AcceptFile.PrivateRules() {
			if len(route.Headers()) > 0 && !r.config.HttpRelayReflectorMode.ReflectsTraffic() {
				panic("ENABLE_RELAY_REFLECTOR must be set to 'all' or 'traffic' to use custom headers in accept files")
			}
		}

		// Rewrite accept file origins through reflector when mode reflects traffic (traffic, all)
		if r.reflector != nil && r.config.HttpRelayReflectorMode.ReflectsTraffic() {

			// Here we loop all the private (incoming) routes and do two things
			// 1. We rewrite the origin to point back to the reflector.  This captures the original URI so
			//    that the reflector can proxy the request to the correct origin.
			// 2. We add any custom headers that are defined in the route, which is a functional addition not available
			//    in the original accept file / snyk-broker.
			//
			// The returned proxyURI is an encoded URI path that has an additional path section which is used
			// to identify the original route and headers.

			for _, route := range renderContext.AcceptFile.PrivateRules() {
				headers := route.Headers()
				routeUri := r.reflector.ProxyURI(route.Origin(), WithHeadersResolver(headers))
				route.SetOrigin(routeUri)
			}
		}
		return nil
	})

	if err != nil {
		r.logger.Error("Error rendering accept file", zap.Error(err))
		return fmt.Errorf("error rendering accept file: %w", err)
	}

	tmpAcceptFile := r.getAcceptFilePath()
	err = os.WriteFile(tmpAcceptFile, rendered, 0644)

	if err != nil {
		fmt.Println("Error writing accept file", err)
		panic(err)
	}

	gen := r.generation.Load()
	done := make(chan struct{})

	// Auto-register: poll for token changes and request restart if changed.
	if r.config.AutoRegisterFrequency != 0 {
		go func() {
			for {
				select {
				case <-time.After(r.config.AutoRegisterFrequency):
					info, err := r.getUrlAndToken()
					if err != nil {
						r.logger.Error("Unable to auto register", zap.Error(err))
						continue
					}
					if info.HasChanged {
						r.requestRestart("token_changed", gen)
					}
				case <-done:
					return
				}
			}
		}()
	}

	// WebSocket tunnel death: request restart when the primus tunnel closes.
	if r.reflector != nil && r.config.HttpRelayReflectorMode.ReflectsRegistration() {
		r.reflector.SetOnWSTunnelClose(func() {
			if os.Getenv("BROKER_RESTART_ON_WEBSOCKET_CLOSE") == "true" {
				r.requestRestart("ws_tunnel_death", gen)
			}
		})
	}

	go func() {

		defer close(done)

		// On any non-clean exit, request a restart so the watchdog
		// picks it up.  Only skip for clean shutdown (!running) or
		// intentional skip (no token / dry run).
		requestRestartOnExit := false
		defer func() {
			if requestRestartOnExit && r.running.Load() {
				r.requestRestart("broker_exit", gen)
			}
		}()

		var info *tokenInfo
		var errx error

		// to allow the agent to start up even if registration isn't available
		// we poll for the URI and Token.  Once we get this we can then
		// start the broker which will generate health and liveness data
		// for the backend.
		for {
			if !r.running.Load() {
				return
			}
			// Check if we've been superseded by a restart (new generation).
			// Without this check, after Restart() calls Start() and sets
			// running=true, old goroutines would continue making registration
			// calls alongside new ones, causing duplicate API calls.
			if r.generation.Load() != gen {
				return
			}
			info, errx = r.getUrlAndToken()

			if errx == errSkipBroker {
				return
			}

			if errx == ErrUnauthorized {
				r.logger.Error("Received 401 Unauthorized from Cortex API, check CORTEX_API_TOKEN is valid.", zap.Error(errx))
				break
			}

			if errx == nil {
				break
			}

			r.logger.Error("Error starting broker, will retry", zap.Error(errx))
			time.Sleep(r.config.FailWaitTime * 5)
		}

		if errx != nil {
			err = errx
			requestRestartOnExit = true
			return
		}

		args := []string{}
		if a := os.Getenv("SNYK_BROKER_ARGS"); a != "" {
			args = strings.Split(a, " ")
		}

		r.logger.Debug("Starting broker",
			zap.String("executable", executable),
			zap.Strings("args", args),
			zap.String("token", info.Token),
			zap.String("uri", info.ServerUri),
			zap.String("acceptFile", tmpAcceptFile),
		)

		brokerEnv := map[string]string{
			"ACCEPT":            tmpAcceptFile,
			"BROKER_SERVER_URL": info.ServerUri,
			"BROKER_TOKEN":      info.Token,
			"PORT":              fmt.Sprintf("%d", r.getSnykBrokerPort()),
		}

		// pick up any env variables that are prefixed with SNYK_BROKER_
		// and add them to the environment
		for _, e := range os.Environ() {
			prefix := "SNYKBROKER_"
			if strings.HasPrefix(e, prefix) {
				parts := strings.SplitN(e, "=", 2)
				if len(parts) != 2 {
					continue
				}
				key := strings.TrimPrefix(parts[0], prefix)
				value := parts[1]
				brokerEnv[key] = value
				r.logger.Debug("Adding SNYKBROKER_ environment variable", zap.String("key", key), zap.String("value", value))
			}
		}

		r.setHttpProxyEnvVars(brokerEnv)

		validationConfig := r.integrationInfo.GetValidationConfig()
		r.applyClientValidationConfig(validationConfig, brokerEnv)

		if r.config.VerboseOutput {
			brokerEnv["LOG_LEVEL"] = "debug"
		}

		r.supervisor = NewSupervisor(
			executable,
			args,
			brokerEnv,
			r.config.FailWaitTime,
		)
		// Only panic on max retries during the very first start.
		// On subsequent restarts, return the error so the recovery
		// loop can retry.
		r.supervisor.panicOnMaxRetries = r.startCount.Load() == 0
		r.startCount.Add(1)
		requestRestartOnExit = true
		supervisor := r.supervisor
		err = supervisor.Start(5, 10*time.Second)
		r.emitOperationCounter("broker_start", err == nil)
		if err != nil {
			r.logger.Warn("Supervisor has exited upon startup", zap.Error(err))
			return
		}
		err = supervisor.Wait()
		r.emitOperationCounter("broker_exit", err == nil)
		if err == nil {
			r.logger.Info("Supervisor has exited")
		} else {
			r.logger.Warn("Supervisor has exited", zap.Error(err))
		}
	}()

	select {
	case <-done:
	case <-time.After(r.config.FailWaitTime):
	}
	return err
}

func (r *relayInstanceManager) setHttpProxyEnvVars(brokerEnv map[string]string) {

	// This is mostly for testing so we can validate no traffic goes out from the broker
	// directly
	if proxyStrictMode := os.Getenv("PROXY_STRICT_MODE"); proxyStrictMode == "true" {
		brokerEnv["HTTP_PROXY"] = "http://not-a-real-proxy:1234"
		brokerEnv["HTTPS_PROXY"] = "http://not-a-real-proxy:1234"
		brokerEnv["NO_PROXY"] = "localhost"
		r.logger.Warn("PROXY_STRICT_MODE is enabled, setting HTTP_PROXY and HTTPS_PROXY to a dummy value")
		return
	}

	httpProxy := os.Getenv("HTTP_PROXY")
	if httpProxy != "" && brokerEnv["HTTP_PROXY"] == "" {
		brokerEnv["HTTP_PROXY"] = httpProxy
	}
	httpsProxy := os.Getenv("HTTPS_PROXY")
	if httpsProxy != "" && brokerEnv["HTTPS_PROXY"] == "" {
		brokerEnv["HTTPS_PROXY"] = httpsProxy
	}

	brokerEnv["NO_PROXY"] = util.EnsureLocalhostNoProxy(false)

	if certPath := r.getCertFilePath(r.config.HttpCaCertFilePath); certPath != "" {
		brokerEnv["NODE_EXTRA_CA_CERTS"] = certPath
	}

	if r.config.HttpDisableTLS {
		brokerEnv["NODE_TLS_REJECT_UNAUTHORIZED"] = "0"
	}
}

func (r *relayInstanceManager) getCertFilePath(certPath string) string {

	if certPath == "" {
		return ""
	}

	stat, err := os.Stat(certPath)

	if err != nil {
		r.logger.Error("Error checking CA cert file", zap.String("path", certPath), zap.Error(err))
		return ""
	}

	// if it's a directory, pick the first .pem file in the directory
	if stat.IsDir() {
		files, err := os.ReadDir(certPath)
		if err != nil {
			r.logger.Error("Error reading CA cert directory", zap.String("path", certPath), zap.Error(err))
			return ""
		}
		for _, file := range files {
			if strings.HasSuffix(file.Name(), ".pem") {
				certPath = path.Join(certPath, file.Name())
				break
			}
		}
	}

	return certPath
}

func (r *relayInstanceManager) applyClientValidationConfig(validationConfig *common.ValidationConfig, brokerEnv map[string]string) {
	if validationConfig != nil {
		brokerEnv["BROKER_CLIENT_VALIDATION_URL"] = validationConfig.URL
		if validationConfig.Method != "" {
			brokerEnv["BROKER_CLIENT_VALIDATION_METHOD"] = validationConfig.Method
		}
		switch validationConfig.Auth.Type {
		case "header":
			brokerEnv["BROKER_CLIENT_VALIDATION_AUTHORIZATION_HEADER"] = validationConfig.Auth.Value
		case "basic":
			brokerEnv["BROKER_CLIENT_VALIDATION_BASIC_AUTH"] = validationConfig.Auth.Value
		}
	}
}

func (r *relayInstanceManager) Close() error {

	if r.running.CompareAndSwap(true, false) {
		s := r.supervisor
		r.supervisor = nil
		if s != nil {
			return s.Close()
		}
	}
	acceptfilePath := r.getAcceptFilePath()
	if _, err := os.Stat(acceptfilePath); !os.IsNotExist(err) {
		os.Remove(acceptfilePath)
	}
	return nil
}
