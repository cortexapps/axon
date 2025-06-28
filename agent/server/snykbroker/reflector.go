package snykbroker

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/cortexapps/axon/common"
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

func (rr *RegistrationReflector) getProxy(targetURI string, isDefault bool, headers common.ResolverMap) (*proxyEntry, error) {

	if targetURI == "" {
		return nil, fmt.Errorf("target URI cannot be empty")
	}

	_, err := rr.Start()
	if err != nil {
		panic(fmt.Sprintf("failed to start registration reflector: %v", err))
	}

	newEntry, err := newProxyEntry(targetURI, isDefault, rr.server.Port(), headers, rr.transport)
	if err != nil {
		return nil, fmt.Errorf("failed to create new proxy entry: %w", err)
	}

	key := newEntry.key()

	entry, exists := rr.targets[key]
	if !exists {
		entry = *newEntry
		rr.targets[key] = entry
		newEntry.addResponseHeader("x-axon-relay-instance", rr.config.InstanceId)

		rr.logger.Info("Registered redirector",
			zap.String("targetURI", entry.TargetURI),
			zap.String("proxyURI", entry.proxyURI),
			zap.Any("headers", headers),
		)
		return &entry, nil
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
		if entry.key() == hash {
			// Found the target URI
			return &entry, remainder, nil
		}
	}

	return nil, "", fmt.Errorf("no proxy entry found for path: %s", proxyPath)
}

type ProxyOption func(*proxyOption)

type proxyOption struct {
	isDefault       bool
	headerResolvers common.ResolverMap
}

func WithDefault(value bool) ProxyOption {
	return func(option *proxyOption) {
		option.isDefault = value
	}
}

func WithHeaders(headers map[string]string) ProxyOption {
	return func(option *proxyOption) {
		if option.headerResolvers == nil {
			option.headerResolvers = make(common.ResolverMap, len(headers))
		}
		for k, v := range headers {
			option.headerResolvers[k] = common.StringValueResolver(v)
		}
	}
}

func WithHeadersResolver(headers common.ResolverMap) ProxyOption {
	return func(option *proxyOption) {
		option.headerResolvers = headers
	}
}

func (rr *RegistrationReflector) ProxyURI(target string, options ...ProxyOption) string {

	opts := &proxyOption{}

	for _, opt := range options {
		opt(opts)
	}

	proxy, err := rr.getProxy(target, opts.isDefault, opts.headerResolvers)
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
	isDefault       bool
	TargetURI       string // Exported for clean access
	proxyURI        string
	handler         http.Handler
	headers         common.ResolverMap
	responseHeaders map[string]string
	hashCode        string
}

func newProxyEntry(targetURI string, isDefault bool, port int, headers common.ResolverMap, transport *http.Transport) (*proxyEntry, error) {
	if targetURI == "" {
		return nil, fmt.Errorf("target URI cannot be empty")
	}

	// Parse the target URI
	asUri, err := url.Parse(targetURI)
	if err != nil {
		return nil, fmt.Errorf("invalid target URI: %w", err)
	}

	// Create the reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(asUri)

	pe := &proxyEntry{
		isDefault: isDefault,
		TargetURI: targetURI,
		handler:   proxy,
		headers:   headers,
	}

	// Set up the director to handle host and headers
	defaultDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		defaultDirector(req)
		req.Host = asUri.Host

		// Copy headers to avoid mutation
		processedHeaders := headers.Resolve()

		// Inject custom headers
		for headerName, headerValue := range processedHeaders {
			req.Header.Set(headerName, headerValue)
		}
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		for headerName, headerValue := range pe.responseHeaders {
			resp.Header.Set(headerName, headerValue)
		}
		return nil
	}

	// Set transport if provided
	if transport != nil {
		proxy.Transport = transport
	}

	pe.proxyURI = pe.encodeProxyUri(targetURI, port, isDefault)

	return pe, nil
}

func (pe *proxyEntry) key() string {
	if pe.isDefault {
		return "default"
	}
	if pe.hashCode == "" {

		key := pe.TargetURI

		if len(pe.headers) > 0 {
			// Create a unique key that includes headers to allow different header sets for the same URI
			headerKey := ""
			for k, v := range pe.headers {
				headerKey += fmt.Sprintf("|%s=%s", k, v())
			}
			key = key + headerKey
		}
		hash := hashString(key)
		pe.hashCode = fmt.Sprintf("%d", hash)
	}
	return pe.hashCode

}

func (pe *proxyEntry) addResponseHeader(name, value string) {
	if pe.responseHeaders == nil {
		pe.responseHeaders = make(map[string]string)
	}
	pe.responseHeaders[name] = value
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
	return fmt.Sprintf("%s/!%s!", baseProxyURI, pe.key())
}
