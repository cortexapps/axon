package snykbroker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const defaultSnykBroker = "snyk-broker"
const brokerPort = 7343

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
	tokenInfo         *tokenInfo
	operationsCounter *prometheus.CounterVec
}

type tokenInfo struct {
	ServerUri  string
	Token      string
	HasChanged bool
}

func (t *tokenInfo) equals(other *tokenInfo) bool {
	if other == nil {
		return false
	}
	if t.ServerUri != other.ServerUri {
		return false
	}
	if t.Token != other.Token {
		return false
	}
	return true
}

func NewRelayInstanceManager(
	lifecycle fx.Lifecycle,
	config config.AgentConfig,
	logger *zap.Logger,
	i common.IntegrationInfo,
	httpServer cortexHttp.Server,
	registration Registration,
	registry *prometheus.Registry,
) RelayInstanceManager {
	mgr := &relayInstanceManager{
		config:          config,
		logger:          logger,
		integrationInfo: i,
		registration:    registration,
		operationsCounter: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "broker_operations",
				Help: "Counter for broker operations",
			},
			[]string{"integration", "alias", "operation", "status"},
		),
	}

	httpServer.RegisterHandler(mgr)

	if registry != nil {
		registry.MustRegister(mgr.operationsCounter)
	}

	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			return mgr.Start()
		},
		OnStop: func(ctx context.Context) error {
			return mgr.Close()
		},
	})
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

	r.logger.Info("Restarting broker")
	err = r.Start()
	if err != nil {
		return fmt.Errorf("unable to start supervisor on Restart: %w", err)
	}
	return nil
}

func (r *relayInstanceManager) getUrlAndToken() (*tokenInfo, error) {
	uri, token, err := r.getUrlAndTokenCore()
	if err != nil {
		return nil, err
	}

	tokenInfo := &tokenInfo{
		ServerUri: uri,
		Token:     token,
	}

	if !tokenInfo.equals(r.tokenInfo) {
		r.logger.Info("Registration info has changed", zap.String("uri", tokenInfo.ServerUri), zap.String("token", tokenInfo.Token))
		tokenInfo.HasChanged = true
		r.tokenInfo = tokenInfo
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

func (r *relayInstanceManager) Start() error {

	if !r.running.CompareAndSwap(false, true) {
		return fmt.Errorf("already started")
	}

	executable := defaultSnykBroker

	if directPath := os.Getenv("SNYK_BROKER_PATH"); directPath != "" {
		executable = directPath
	}

	acceptFile, err := r.integrationInfo.AcceptFile()

	if err != nil {
		fmt.Println("Error getting accept file", err)
		panic(err)
	}

	done := make(chan struct{})

	if r.config.AutoRegisterFrequency != 0 {
		go func() {
			for {
				if !r.running.Load() {
					return
				}
				select {
				case <-time.After(r.config.AutoRegisterFrequency):

					info, err := r.getUrlAndToken()
					if err != nil {
						r.logger.Error("Unable to auto register", zap.Error(err))
						continue
					}
					if info.HasChanged {
						r.logger.Info("Auto registered broker, token has changed, restarting")
						err = r.Restart()
						if err != nil {
							r.logger.Error("Unable to auto register restart", zap.Error(err))
							continue
						}
					}
				case <-done:
					return
				}
			}
		}()
	}

	go func() {

		defer close(done)

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
			// In this case we will fail out of start which will shut down
			// initialization and exit.
			err = errx
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
			zap.String("acceptFile", acceptFile),
		)

		brokerEnv := map[string]string{
			"ACCEPT":            acceptFile,
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
		r.startCount.Add(1)
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

	httpProxy := os.Getenv("HTTP_PROXY")
	if httpProxy != "" && brokerEnv["HTTP_PROXY"] == "" {
		brokerEnv["HTTP_PROXY"] = httpProxy
	}
	httpsProxy := os.Getenv("HTTPS_PROXY")
	if httpsProxy != "" && brokerEnv["HTTPS_PROXY"] == "" {
		brokerEnv["HTTPS_PROXY"] = httpsProxy
	}
	noProxy := os.Getenv("NO_PROXY")
	if noProxy != "" && brokerEnv["NO_PROXY"] == "" {
		brokerEnv["NO_PROXY"] = noProxy
	}

	if r.config.HttpCaCertFilePath != "" {
		brokerEnv["NODE_EXTRA_CA_CERTS"] = r.config.HttpCaCertFilePath
	}
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
	return nil
}
