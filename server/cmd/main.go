package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/cortexapps/axon-server/adapters"
	"github.com/cortexapps/axon-server/broker"
	"github.com/cortexapps/axon-server/config"
	"github.com/cortexapps/axon-server/dispatch"
	"github.com/cortexapps/axon-server/metrics"
	"github.com/cortexapps/axon-server/tunnel"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

func main() {
	cfg := config.NewConfigFromEnv()

	// Set up structured JSON logging.
	zapCfg := zap.NewProductionConfig()
	zapCfg.EncoderConfig.TimeKey = "time"
	zapCfg.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		enc.AppendString(t.UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	if os.Getenv("ENV") != "production" {
		zapCfg = zap.NewDevelopmentConfig()
	}
	logger, err := zapCfg.Build()
	if err != nil {
		panic(err)
	}
	logger = logger.Named("axon-tunnel-server")
	defer logger.Sync()

	cfg.Print()

	// Initialize metrics.
	m := metrics.New(cfg.ServerID)
	defer m.Closer()

	// Initialize BROKER_SERVER client.
	brokerClient := broker.NewClient(cfg.BrokerServerURL, cfg.ServerID, logger)

	// Initialize client registry and tunnel service.
	registry := tunnel.NewClientRegistry(logger)
	tunnelService := tunnel.NewService(cfg, logger, registry, brokerClient, m)

	// Create gRPC server with keepalive.
	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             15 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	pb.RegisterTunnelServiceServer(grpcServer, tunnelService)

	// Start gRPC listener.
	grpcLis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.GrpcPort))
	if err != nil {
		logger.Fatal("Failed to listen for gRPC", zap.Error(err))
	}

	// Initialize dispatcher and HTTP adapter, and wire frame delivery.
	dispatcher := dispatch.NewDispatcher(cfg, registry, m, logger)
	tunnelService.SetFrameHandler(dispatcher.HandleFrame)
	tunnelService.SetStreamCloseHandler(dispatcher.HandleStreamClose)
	httpAdapter := adapters.NewHttpAdapter(cfg, registry, dispatcher, m, logger)

	// Start HTTP server for metrics, health, and dispatch.
	httpMux := http.NewServeMux()
	httpMux.Handle("/metrics", m.Handler())
	httpMux.Handle("/broker/", httpAdapter)
	httpMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","server_id":%q,"clients":%d,"streams":%d,"inflight":%d,"broker_server_configured":%t}`,
			cfg.ServerID, registry.Count(), registry.StreamCount(), dispatcher.InflightCount(), brokerClient.IsConfigured())
	})

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HttpPort),
		Handler: httpMux,
	}

	// Notify BROKER_SERVER that this server instance has started. The
	// client retries transient failures internally; the outer loop adds
	// unbounded persistence for longer outages.
	if brokerClient.IsConfigured() {
		go func() {
			backoff := 5 * time.Second
			for {
				if err := brokerClient.ServerStarting(context.Background(), cfg.ServerID); err != nil {
					logger.Warn("BROKER_SERVER server-starting exhausted retries, will try again",
						zap.Error(err), zap.Duration("backoff", backoff))
					time.Sleep(backoff)
					backoff = min(backoff*2, time.Minute)
					continue
				}
				logger.Info("BROKER_SERVER server-starting succeeded")
				break
			}
		}()
	}

	// Start periodic re-registration of all active clients.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if brokerClient.IsConfigured() {
		go func() {
			ticker := time.NewTicker(cfg.ReRegistrationInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					registry.ForEach(func(token broker.Token, identity tunnel.ClientIdentity) {
						if err := brokerClient.ClientConnected(ctx, token, identity.InstanceID, nil); err != nil {
							logger.Warn("Periodic re-registration failed",
								zap.String("tenantId", identity.TenantID),
								zap.Error(err))
						}
					})
				}
			}
		}()
	}

	// Start servers.
	go func() {
		logger.Info("Starting HTTP server", zap.Int("port", cfg.HttpPort))
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP server failed", zap.Error(err))
		}
	}()

	go func() {
		logger.Info("Starting gRPC server", zap.Int("port", cfg.GrpcPort))
		if err := grpcServer.Serve(grpcLis); err != nil {
			logger.Fatal("gRPC server failed", zap.Error(err))
		}
	}()

	// Wait for shutdown signal.
	<-ctx.Done()
	logger.Info("Shutting down...")

	// Graceful shutdown.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Notify BROKER_SERVER of shutdown before draining (best-effort).
	if brokerClient.IsConfigured() {
		if err := brokerClient.ServerStopping(shutdownCtx); err != nil {
			logger.Warn("BROKER_SERVER server-stopping failed", zap.Error(err))
		} else {
			logger.Info("BROKER_SERVER server-stopping succeeded")
		}
	}

	// Stop accepting new connections and drain.
	grpcServer.GracefulStop()
	httpServer.Shutdown(shutdownCtx)

	logger.Info("Server stopped")
}
