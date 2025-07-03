package acceptfile

import (
	"encoding/json"
	"fmt"

	"github.com/cortexapps/axon/config"
	"go.uber.org/zap"
)

type AcceptFile struct {
	wrapper acceptFileWrapper
	content []byte
	options acceptFileOptions
}

type acceptFileOptions struct {
	Config config.AgentConfig
}

type AcceptFileOption func(*acceptFileOptions)

func WithAgentConfig(cfg config.AgentConfig) AcceptFileOption {
	return func(opts *acceptFileOptions) {
		opts.Config = cfg // Copy the config to avoid modifying the original
	}
}

func NewAcceptFile(content []byte, opts ...AcceptFileOption) *AcceptFile {
	options := acceptFileOptions{
		Config: config.NewAgentEnvConfig(),
	}
	for _, opt := range opts {
		opt(&options)
	}

	// Pre-process content to handle plugin invocations
	processedContent, err := preProcessContent(content)
	if err != nil {
		panic(fmt.Errorf("failed to preprocess accept file content: %w", err))
	}

	af := &AcceptFile{
		content: processedContent,
		options: options,
	}
	af.wrapper = newAcceptFileWrapper(processedContent, af)
	return af
}

func (a *AcceptFile) Validate() error {
	return ensureAcceptFileVars(string(a.content))
}

type RenderContext struct {
	AcceptFile acceptFileWrapper
	Logger     *zap.Logger
}

type RenderStep func(renderContext RenderContext) error

var IgnoreHosts = []string{
	"localhost",
	"127.0.0.1",
}

func (a *AcceptFile) Render(logger *zap.Logger, extraRenderSteps ...RenderStep) ([]byte, error) {

	err := a.Validate()
	if err != nil {
		logger.Error("failed to validate accept file", zap.Error(err))
		return nil, err
	}

	renderContext := RenderContext{
		Logger:     logger,
		AcceptFile: newAcceptFileWrapper(a.content, a),
	}

	renderSteps := append([]RenderStep{
		a.ensurePublicAndPrivate,
		a.addAxonRoute,
	}, extraRenderSteps...)

	for _, step := range renderSteps {
		if err := step(renderContext); err != nil {
			logger.Error("failed to render accept file", zap.Error(err))
			return nil, err
		}
	}

	json, err := renderContext.AcceptFile.toJSON()
	if err != nil {
		logger.Error("failed to marshal accept file content", zap.Error(err))
		return nil, err
	}

	return json, nil

}

func (a *AcceptFile) ensurePublicAndPrivate(renderContext RenderContext) error {
	renderContext.AcceptFile.Routes("public")
	renderContext.AcceptFile.Routes("private")
	return nil
}

func (a *AcceptFile) addAxonRoute(renderContext RenderContext) error {
	// we add a section like this to allow the server side
	// to hit the axon agent via an HTTP bridge
	// "private": [
	// 	{
	// 	  "method": "any",
	// 	  "path": "/__axon/*",
	// 	  "origin": "http://localhost"
	// 	}
	//   ]

	entry := acceptFileRule{
		Method: "any",
		Path:   "/__axon/*",
		Origin: a.options.Config.HttpBaseUrl(),
	}

	renderContext.AcceptFile.AddRoute("private", entry)
	return nil
}

type acceptFileWrapper struct {
	dict       map[string]any
	acceptFile *AcceptFile
}

func newAcceptFileWrapper(content []byte, af *AcceptFile) acceptFileWrapper {
	dict := make(map[string]any)
	err := json.Unmarshal(content, &dict)
	if err != nil {
		panic(fmt.Errorf("failed to unmarshal accept file content: %w", err))
	}

	return acceptFileWrapper{dict: dict, acceptFile: af}
}

func (w acceptFileWrapper) Routes(routeType string) []acceptFileRouteWrapper {
	routesEntry, ok := w.dict[routeType].([]interface{})
	if !ok {
		routesEntry = []any{}
		w.dict[routeType] = routesEntry
	}

	routes := make([]acceptFileRouteWrapper, len(routesEntry))
	for i, route := range routesEntry {
		routeDict, ok := route.(map[string]any)
		if !ok {
			return nil
		}
		routes[i] = acceptFileRouteWrapper{
			dict:       routeDict,
			acceptFile: w.acceptFile,
		}
	}
	return routes
}

func (w acceptFileWrapper) AddRoute(routeType string, entry acceptFileRule) acceptFileRouteWrapper {
	routeAsJson, err := json.Marshal(entry)
	if err != nil {
		panic(fmt.Errorf("failed to marshal accept file route: %w", err))
	}
	var routeDict map[string]any
	err = json.Unmarshal(routeAsJson, &routeDict)
	if err != nil {
		panic(fmt.Errorf("failed to unmarshal accept file route: %w", err))
	}
	existingRoutes := w.dict[routeType].([]any)
	w.dict[routeType] = append([]any{routeDict}, existingRoutes...)
	return acceptFileRouteWrapper{dict: routeDict}
}

func (w acceptFileWrapper) toJSON() ([]byte, error) {
	jsonData, err := json.Marshal(w.dict)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal accept file content: %w", err)
	}
	return jsonData, nil
}

type acceptFileRouteWrapper struct {
	dict       map[string]any
	acceptFile *AcceptFile
}

func (r acceptFileRouteWrapper) Origin() string {
	origin, ok := r.dict["origin"].(string)
	if !ok {
		return ""
	}
	return origin
}

func (r acceptFileRouteWrapper) Path() string {
	path, ok := r.dict["path"].(string)
	if !ok {
		return ""
	}
	return path
}

func (r acceptFileRouteWrapper) SetOrigin(origin string) {
	r.dict["origin"] = origin
}

func (r acceptFileRouteWrapper) Headers() ResolverMap {
	headers, ok := r.dict["headers"].(map[string]any)
	if !ok {
		return nil
	}

	result := make(ResolverMap)
	for k, v := range headers {
		if str, ok := v.(string); ok {
			result[k] = CreateResolver(str, zap.NewNop(), r.acceptFile.options.Config.PluginDirs)
		}
	}
	return result
}

type acceptFileRuleAuth struct {
	Scheme   string `json:"scheme"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
}

type acceptFileRule struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Origin  string              `json:"origin"`
	Auth    *acceptFileRuleAuth `json:"auth,omitempty"`
	Headers map[string]string   `json:"headers,omitempty"`
}
