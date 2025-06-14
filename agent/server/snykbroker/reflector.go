package snykbroker

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/cortexapps/axon/config"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type RegistrationReflector struct {
	logger        *zap.Logger
	transport     *http.Transport
	server        cortexHttp.Server
	targets       map[string]proxyEntry
	serverStarted atomic.Bool
	mode          config.RelayReflectorMode
	config        config.AgentConfig
}

type RegistrationReflectorParams struct {
	fx.In
	Lifecycle fx.Lifecycle `optional:"true"`
	Logger    *zap.Logger
	Transport *http.Transport      `optional:"true"`
	Registry  *prometheus.Registry `optional:"true"`
	Config    config.AgentConfig
}

func NewRegistrationReflector(p RegistrationReflectorParams) *RegistrationReflector {

	httpParams := cortexHttp.HttpServerParams{
		Logger: p.Logger.Named("relay-reflector"),
	}

	if p.Registry != nil {
		httpParams.Registry = p.Registry
	}

	server := cortexHttp.NewHttpServer(httpParams, cortexHttp.WithName("relay-reflector"))

	rr := &RegistrationReflector{
		transport: p.Transport,
		server:    server,
		logger:    httpParams.Logger,
		targets:   make(map[string]proxyEntry),
		mode:      p.Config.HttpRelayReflectorMode,
		config:    p.Config,
	}

	server.RegisterHandler(rr)

	if p.Lifecycle != nil {
		p.Lifecycle.Append(
			fx.Hook{
				OnStart: func(ctx context.Context) error {
					_, err := rr.Start()
					return err
				},
			},
		)
	}

	return rr
}

func (rr *RegistrationReflector) Start() (int, error) {

	if rr.serverStarted.CompareAndSwap(false, true) {
		_, err := rr.server.Start()
		if err != nil {
			return 0, err
		}
	}

	return rr.server.Port(), nil
}

func (rr *RegistrationReflector) Stop() error {
	if rr.server != nil {
		return rr.server.Close()
	}
	return nil
}

func (rr *RegistrationReflector) getProxy(targetURI string, isDefault bool) (*proxyEntry, error) {
	if targetURI == "" {
		return nil, fmt.Errorf("target URI cannot be empty")
	}

	key := targetURI

	if isDefault {
		key = "default"
	}

	entry, exists := rr.targets[key]
	if !exists {

		_, err := rr.Start()
		if err != nil {
			panic(fmt.Sprintf("failed to start registration reflector: %v", err))
		}

		asUri, err := url.Parse(targetURI)
		if err != nil {
			return nil, fmt.Errorf("invalid target URI: %w", err)
		}

		proxy := httputil.NewSingleHostReverseProxy(asUri)

		// The proxy needs to override the Host and the URL host to not get erroneous 404s
		// https://stackoverflow.com/questions/23164547/golang-reverseproxy-not-working
		// https://github.com/golang/go/issues/14413
		defaultDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			defaultDirector(req)
			req.Host = asUri.Host
		}
		proxy.ModifyResponse = func(resp *http.Response) error {
			resp.Header.Set("x-axon-relay-instance", rr.config.InstanceId)
			return nil
		}

		if rr.transport != nil {
			proxy.Transport = rr.transport
		}

		newEntry := newProxyEntry(targetURI, isDefault, proxy, rr.server.Port())
		rr.targets[key] = *newEntry
		entry = *newEntry

		rr.logger.Info("Registered redirector",
			zap.String("targetURI", entry.targetURI),
			zap.String("proxyURI", entry.proxyURI),
		)
	}
	return &entry, nil
}

func (rr *RegistrationReflector) extractHash(part string) string {
	// Check if the part is a valid hash (a number)
	if !strings.HasPrefix(part, "!") || !strings.HasSuffix(part, "!") {
		return ""
	}
	return part[1 : len(part)-1]
}

func (rr *RegistrationReflector) parseTargetUri(proxyPath string) (*proxyEntry, string, error) {
	path := strings.TrimLeft(proxyPath, "/")
	slash := strings.Index(path, "/")
	beforeSlash := path
	remainder := "/"
	if slash != -1 {
		beforeSlash = path[:slash]
		remainder = path[slash:]
	}
	hash := rr.extractHash(beforeSlash)
	if hash == "" {
		// find the default proxy entry
		if entry, exists := rr.targets["default"]; exists {
			// Found the default proxy entry
			return &entry, proxyPath, nil
		} else {
			// No default proxy entry found, return an error
			return nil, "", fmt.Errorf("no default proxy entry found for path: %s", proxyPath)
		}
	}

	for _, entry := range rr.targets {
		if entry.hashCode == hash {
			// Found the target URI
			return &entry, remainder, nil
		}
	}

	return nil, "", fmt.Errorf("no proxy entry found for path: %s", proxyPath)
}

type ProxyOption func(*proxyOption)

type proxyOption struct {
	isDefault bool
}

func WithDefault(value bool) ProxyOption {
	return func(option *proxyOption) {
		option.isDefault = value
	}
}

func (rr *RegistrationReflector) ProxyURI(target string, options ...ProxyOption) string {

	opts := &proxyOption{}

	for _, opt := range options {
		opt(opts)
	}

	proxy, err := rr.getProxy(target, opts.isDefault)
	if err != nil {
		rr.logger.Error("Failed to get proxy URI", zap.Error(err))
		return target
	}
	return proxy.proxyURI
}

func (rr *RegistrationReflector) RegisterRoutes(mux *mux.Router) error {
	mux.PathPrefix("/").Handler(rr)
	return nil
}

func (rr *RegistrationReflector) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	entry, newPath, err := rr.parseTargetUri(r.URL.Path)
	if err != nil {
		rr.logger.Error("Failed to parse target URI", zap.Error(err))
		http.Error(w, "Invalid target URI", http.StatusBadGateway)
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	if !strings.HasPrefix(newPath, "/") {
		newPath = "/" + newPath
	}
	r.URL.Path = newPath
	entry.handler.ServeHTTP(w, r)
}

func hashString(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

type proxyEntry struct {
	isDefault bool
	targetURI string
	hashCode  string
	proxyURI  string
	handler   http.Handler
}

func newProxyEntry(targetURI string, isDefault bool, handler http.Handler, port int) *proxyEntry {

	pe := &proxyEntry{
		isDefault: isDefault,
		targetURI: targetURI,
		hashCode:  strconv.Itoa(int(hashString(targetURI))),
		handler:   handler,
	}
	pe.proxyURI = pe.encodeProxyUri(targetURI, port, isDefault)

	return pe
}

func (pe *proxyEntry) encodeProxyUri(targetURI string, port int, isDefault bool) string {
	baseProxyURI := fmt.Sprintf("http://localhost:%d", port)
	if isDefault {
		// for default proxy, we only change the host and port
		// to be our proxy
		parsedProxyURI, err := url.Parse(baseProxyURI)
		if err != nil {
			panic(fmt.Sprintf("failed to parse proxy URI %q: %v", baseProxyURI, err))
		}

		parsedTarget, err := url.Parse(targetURI)
		if err != nil {
			panic(fmt.Errorf("failed to parse target URI %q: %v", targetURI, err))
		}
		parsedTarget.Host = parsedProxyURI.Host
		parsedTarget.Scheme = parsedProxyURI.Scheme
		return parsedTarget.String()
	}
	return fmt.Sprintf("%s/!%d!", baseProxyURI, hashString(targetURI))
}
