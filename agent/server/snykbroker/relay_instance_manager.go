package snykbroker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const defaultSnykBroker = "snyk-broker"

type RelayInstanceManager interface {
	Start() error
	Close() error
}

type relayInstanceManager struct {
	integrationInfo common.IntegrationInfo
	registration    Registration
	config          config.AgentConfig
	logger          *zap.Logger
	supervisor      *Supervisor
	running         atomic.Bool
	startCount      atomic.Int32
}

func NewRelayInstanceManager(
	lifecycle fx.Lifecycle,
	config config.AgentConfig,
	logger *zap.Logger,
	i common.IntegrationInfo,
	httpServer cortexHttp.Server,
	registration Registration,
) RelayInstanceManager {
	mgr := &relayInstanceManager{
		config:          config,
		logger:          logger,
		integrationInfo: i,
		registration:    registration,
	}

	httpServer.RegisterHandler(mgr)

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

func (r *relayInstanceManager) RegisterRoutes(mux *http.ServeMux) error {
	mux.Handle(fmt.Sprintf("%s/reregister", cortexHttp.AxonPathRoot), r)
	return nil
}

func (r *relayInstanceManager) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	err := r.restart()
	if err != nil {
		r.logger.Error("Unable to reregister", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Unable to reregister"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

var errSkipBroker = errors.New("NoBrokerToken")

func (r *relayInstanceManager) restart() error {
	// re-register and restart supervisor
	err := r.Close()
	if err != nil {
		r.logger.Error("unable to close supervisor on /reregister", zap.Error(err))
	}

	err = r.Start()
	if err != nil {
		return fmt.Errorf("unable to start supervisor on /reregister: %w", err)
	}
	return nil
}

func (r *relayInstanceManager) getUrlAndToken() (string, string, error) {

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
	if err != nil {
		return "", "", err
	}

	return reg.ServerUri, reg.Token, nil
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

	go func() {

		defer close(done)

		var uri string
		var token string
		var errx error

		// to allow the agent to start up even if registration isn't available
		// we poll for the URI and Token.  Once we get this we can then
		// start the broker which will generate health and liveness data
		// for the backend.
		for {
			if !r.running.Load() {
				return
			}
			uri, token, errx = r.getUrlAndToken()

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
			zap.String("token", token),
			zap.String("uri", uri),
			zap.String("acceptFile", acceptFile),
		)

		r.supervisor = NewSupervisor(
			executable,
			args,
			map[string]string{
				"ACCEPT":            acceptFile,
				"BROKER_SERVER_URL": uri,
				"BROKER_TOKEN":      token,
				"PORT":              "7343",
			},
			r.config.FailWaitTime,
		)
		r.startCount.Add(1)
		supervisor := r.supervisor
		err = supervisor.Start(5, 10*time.Second)
		if err != nil {
			r.logger.Warn("Supervisor has exited upon startup", zap.Error(err))
			return
		}
		err = supervisor.Wait()
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
