package snykbroker

import (
	"context"
	"crypto/tls"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/cortexapps/axon/config"
	cortexHttp "github.com/cortexapps/axon/server/http"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
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
		Config: p.Config,
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

func (rr *RegistrationReflector) getProxy(targetURI string, isDefault bool, headers acceptfile.ResolverMap) (*proxyEntry, error) {

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
			zap.Bool("isDefault", entry.isDefault),
			zap.String("key", key),
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
	headerResolvers acceptfile.ResolverMap
}

func WithDefault(value bool) ProxyOption {
	return func(option *proxyOption) {
		option.isDefault = value
	}
}

func WithHeaders(headers map[string]string) ProxyOption {
	return func(option *proxyOption) {
		if option.headerResolvers == nil {
			option.headerResolvers = make(acceptfile.ResolverMap, len(headers))
		}
		for k, v := range headers {
			option.headerResolvers[k] = acceptfile.StringValueResolver(v)
		}
	}
}

func WithHeadersResolver(headers acceptfile.ResolverMap) ProxyOption {
	return func(option *proxyOption) {
		option.headerResolvers = headers
	}
}

func (rr *RegistrationReflector) getUriForTarget(target string) (string, error) {

	if target == "" {
		return "", fmt.Errorf("target URI cannot be empty")
	}

	for _, entry := range rr.targets {
		if entry.TargetURI == target {
			return entry.proxyURI, nil
		}
	}
	return "", fmt.Errorf("no proxy entry found for target URI: %s", target)
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

	rr.logger.Debug("Received request for proxy",
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
	)
	entry, newPath, err := rr.parseTargetUri(r.URL.Path)
	if err != nil {
		rr.logger.Error("Failed to find Entry for target URI", zap.Error(err))
		http.Error(w, "Invalid target URI", http.StatusBadGateway)
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	if !strings.HasPrefix(newPath, "/") {
		newPath = "/" + newPath
	}
	r.URL.Path = newPath
	rr.logger.Debug("Proxying request",
		zap.String("targetURI", entry.TargetURI),
		zap.String("proxyURI", entry.proxyURI),
		zap.String("key", entry.key()),
		zap.String("newPath", newPath),
	)

	// Check if this is a WebSocket upgrade request
	if rr.isWebSocketUpgrade(r) {
		rr.logger.Debug("Detected WebSocket upgrade request, using WebSocket proxy")
		rr.proxyWebSocket(w, r, entry)
		return
	}

	entry.handler.ServeHTTP(w, r)
}

// isWebSocketUpgrade checks if the request is a WebSocket upgrade request
// and if WebSocket upgrade support is enabled in config
func (rr *RegistrationReflector) isWebSocketUpgrade(r *http.Request) bool {
	if !rr.config.ReflectorWebSocketUpgrade {
		return false
	}
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// proxyWebSocket handles WebSocket upgrade requests by establishing a tunnel
func (rr *RegistrationReflector) proxyWebSocket(w http.ResponseWriter, r *http.Request, entry *proxyEntry) {
	// Parse target URL
	targetURL, err := url.Parse(entry.TargetURI)
	if err != nil {
		rr.logger.Error("Failed to parse target URI for WebSocket", zap.Error(err))
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	// Determine target host and port
	targetHost := targetURL.Host
	targetPort := "80"
	if targetURL.Scheme == "https" || targetURL.Scheme == "wss" {
		targetPort = "443"
	}
	if h, p, err := net.SplitHostPort(targetURL.Host); err == nil {
		targetHost = h
		targetPort = p
	}

	// Connect to the target server
	var targetConn net.Conn
	targetAddr := net.JoinHostPort(targetHost, targetPort)

	if targetURL.Scheme == "https" || targetURL.Scheme == "wss" {
		tlsConfig := &tls.Config{
			ServerName: targetHost,
		}
		if rr.config.HttpDisableTLS {
			tlsConfig.InsecureSkipVerify = true
		}
		targetConn, err = tls.Dial("tcp", targetAddr, tlsConfig)
	} else {
		targetConn, err = net.Dial("tcp", targetAddr)
	}

	if err != nil {
		rr.logger.Error("Failed to connect to target for WebSocket",
			zap.String("target", targetAddr),
			zap.Error(err))
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	// Build the upgrade request to send to target
	r.URL.Scheme = targetURL.Scheme
	r.URL.Host = targetURL.Host
	r.Host = targetURL.Host

	// Write the request to the target
	if err := r.Write(targetConn); err != nil {
		rr.logger.Error("Failed to write WebSocket upgrade request to target", zap.Error(err))
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	// Hijack the client connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		rr.logger.Error("ResponseWriter does not support hijacking")
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		rr.logger.Error("Failed to hijack client connection", zap.Error(err))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	rr.logger.Debug("WebSocket tunnel established",
		zap.String("target", targetAddr),
		zap.String("path", r.URL.Path))

	// Bidirectionally copy data between client and target
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(clientConn, targetConn)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(targetConn, clientConn)
		done <- struct{}{}
	}()

	// Wait for either direction to complete
	<-done
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
	headers         acceptfile.ResolverMap
	responseHeaders map[string]string
	hashCode        string
}

func newProxyEntry(targetURI string, isDefault bool, port int, headers acceptfile.ResolverMap, transport *http.Transport) (*proxyEntry, error) {
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
		processedHeaders := headers.ToStringMap()

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
			for k := range pe.headers {
				headerKey += fmt.Sprintf("|%s=%s", k, pe.headers.ResolverKey(k))
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
