package acceptfile

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/cortexapps/axon/config"
	axonHttp "github.com/cortexapps/axon/server/http"
	"go.uber.org/zap"
)

// AcceptFile is an abstraction over the Snyk Broker "accept.json" format.
// It owns manipulating a raw file into one that is customized for Axon including
// adding the Axon rules, and doing replacements for the reflector (eg all traffic is sent through the agent
// instead of directly to the target), as well as adding support for adding outbound headers.
type AcceptFile struct {
	wrapper acceptFileWrapper
	content []byte
	config  config.AgentConfig
	logger  *zap.Logger
}

// NewAcceptFile creates a new AcceptFile instance, taking the raw content of the accept file
// and the agent configuration. It preprocesses the content to handle plugin invocations.
func NewAcceptFile(content []byte, cfg config.AgentConfig, logger *zap.Logger) (*AcceptFile, error) {

	// Fixup ${} references to support plugins without confusing with env vars
	processedContent, err := preProcessContent(content)
	if err != nil {
		return nil, fmt.Errorf("failed to preprocess accept file content: %w", err)
	}

	if logger == nil {
		logger = zap.NewNop()
	}
	af := &AcceptFile{
		content: processedContent,
		config:  cfg,
		logger:  logger.Named("accept-file"),
	}

	if err := ensureAcceptFileVars(string(af.content)); err != nil {
		return nil, err
	}

	af.wrapper = newAcceptFileWrapper(processedContent, af)
	return af, nil
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

const RULES_PRIVATE = "private"
const RULES_PUBLIC = "public"

// Render renders the accept file by applying Axon updates plus any additional render steps provided.
// It returns the rendered JSON content of the accept file.
func (a *AcceptFile) Render(logger *zap.Logger, extraRenderSteps ...RenderStep) ([]byte, error) {

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
	renderContext.AcceptFile.PrivateRules()
	renderContext.AcceptFile.PublicRules()
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
		Path:   fmt.Sprintf("%s/*", axonHttp.AxonPathRoot),
		Origin: a.config.HttpBaseUrl(),
	}

	renderContext.AcceptFile.AddRule(RULES_PRIVATE, entry)
	return nil
}

//
// The wrapper classes below provide strongly typed access to the accept file
// which is parsed as a map[string]any.  We do this to ensure full compatibility
// with the Snyk Broker accept file format while also being able to add extra functionality.
// In other words, if we simply mapped the file to a struct here, its possible there would be content
// in the accept file that we don't know about, and we would lose that content on a round trip.
//

// acceptFileWrapper provides a strongly typed wrapper around the accept file content.
type acceptFileWrapper struct {
	dict       map[string]any
	acceptFile *AcceptFile
}

func newAcceptFileWrapper(content []byte, af *AcceptFile) acceptFileWrapper {
	dict := make(map[string]any)
	err := json.Unmarshal(content, &dict)
	if err != nil {
		panic(fmt.Errorf("failed to unmarshal accept file content: %w, content was:\n%s", err, string(content)))
	}

	return acceptFileWrapper{dict: dict, acceptFile: af}
}

func (w acceptFileWrapper) PrivateRules() []acceptFileRuleWrapper {
	return w.rules(RULES_PRIVATE)
}

func (w acceptFileWrapper) PublicRules() []acceptFileRuleWrapper {
	return w.rules(RULES_PUBLIC)
}

func (w acceptFileWrapper) rules(routeType string) []acceptFileRuleWrapper {
	routesEntry, ok := w.dict[routeType].([]interface{})
	if !ok {
		routesEntry = []any{}
		w.dict[routeType] = routesEntry
	}

	routes := make([]acceptFileRuleWrapper, len(routesEntry))
	for i, route := range routesEntry {
		routeDict, ok := route.(map[string]any)
		if !ok {
			return nil
		}
		routes[i] = acceptFileRuleWrapper{
			dict:       routeDict,
			acceptFile: w.acceptFile,
		}
	}
	return routes
}

// AddRule adds a new route to the accept file for the specified route type.
func (w acceptFileWrapper) AddRule(routeType string, entry acceptFileRule) acceptFileRuleWrapper {

	// with a little extra work here we could probably just directly use
	// the entry structure above, but the acceptFileRuleWrapper takes a dict so we need
	// to convert it to a map[string]any first, so we round trip it through JSON.

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
	return acceptFileRuleWrapper{dict: routeDict}
}

func (w acceptFileWrapper) toJSON() ([]byte, error) {
	jsonData, err := json.Marshal(w.dict)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal accept file content: %w", err)
	}
	return jsonData, nil
}

type acceptFileRuleWrapper struct {
	dict       map[string]any
	acceptFile *AcceptFile
}

func (r acceptFileRuleWrapper) Origin() string {
	origin, ok := r.dict["origin"].(string)
	if !ok {
		return ""
	}
	return os.ExpandEnv(origin)
}

func (r acceptFileRuleWrapper) Path() string {
	path, ok := r.dict["path"].(string)
	if !ok {
		return ""
	}
	return path
}

func (r acceptFileRuleWrapper) SetOrigin(origin string) {
	r.dict["origin"] = origin
}

func (r acceptFileRuleWrapper) Headers() ResolverMap {
	headers, ok := r.dict["headers"].(map[string]any)
	if !ok {
		return nil
	}

	result := make(ResolverMap)
	for k, v := range headers {
		if str, ok := v.(string); ok {
			result[k] = CreateResolver(str, r.acceptFile.logger, r.acceptFile.config.PluginDirs)
		}
	}
	return result
}

// Here are our JSON structed types that represent the accept file rules.
// that we can use for things that we are generating such that we don't need to worry
// about additional fields that might be in the accept file that we don't know about.

type acceptFileRule struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Origin  string              `json:"origin"`
	Auth    *acceptFileRuleAuth `json:"auth,omitempty"`
	Headers map[string]string   `json:"headers,omitempty"`
}

type acceptFileRuleAuth struct {
	Scheme   string `json:"scheme"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
}
