package acceptfile

import (
	"encoding/json"
	"fmt"
	"net/url"
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

// Wrapper returns the typed wrapper for accessing accept file rules.
func (a *AcceptFile) Wrapper() acceptFileWrapper {
	return a.wrapper
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

func (w acceptFileWrapper) PrivateRules() []AcceptFileRuleWrapper {
	return w.rules(RULES_PRIVATE)
}

func (w acceptFileWrapper) PublicRules() []AcceptFileRuleWrapper {
	return w.rules(RULES_PUBLIC)
}

func (w acceptFileWrapper) rules(routeType string) []AcceptFileRuleWrapper {
	routesEntry, ok := w.dict[routeType].([]interface{})
	if !ok {
		routesEntry = []any{}
		w.dict[routeType] = routesEntry
	}

	routes := make([]AcceptFileRuleWrapper, len(routesEntry))
	for i, route := range routesEntry {
		routeDict, ok := route.(map[string]any)
		if !ok {
			return nil
		}
		routes[i] = AcceptFileRuleWrapper{
			dict:       routeDict,
			acceptFile: w.acceptFile,
		}
	}
	return routes
}

// AddRule adds a new route to the accept file for the specified route type.
func (w acceptFileWrapper) AddRule(routeType string, entry acceptFileRule) AcceptFileRuleWrapper {

	// with a little extra work here we could probably just directly use
	// the entry structure above, but the AcceptFileRuleWrapper takes a dict so we need
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
	return AcceptFileRuleWrapper{dict: routeDict}
}

func (w acceptFileWrapper) toJSON() ([]byte, error) {
	jsonData, err := json.Marshal(w.dict)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal accept file content: %w", err)
	}
	return jsonData, nil
}

// AcceptFileRuleWrapper provides strongly typed access to a single accept file rule
// parsed from the raw JSON dictionary.
type AcceptFileRuleWrapper struct {
	dict       map[string]any
	acceptFile *AcceptFile
}

func (r AcceptFileRuleWrapper) Origin() string {
	rawOrigin, ok := r.dict["origin"].(string)
	if !ok {
		return ""
	}
	origin := os.ExpandEnv(rawOrigin)
	asUrl, err := url.Parse(origin)
	if err != nil {
		r.acceptFile.logger.Panic("failed to parse origin URL", zap.String("origin", rawOrigin), zap.Error(err))
	}
	if asUrl.Scheme == "" {
		r.acceptFile.logger.Warn("origin URL has no scheme, defaulting to https", zap.String("origin", rawOrigin))
		asUrl.Scheme = "https"
		return asUrl.String()
	}
	return origin

}

func (r AcceptFileRuleWrapper) Path() string {
	path, ok := r.dict["path"].(string)
	if !ok {
		return ""
	}
	return path
}

func (r AcceptFileRuleWrapper) SetOrigin(origin string) {
	r.dict["origin"] = origin
}

func (r AcceptFileRuleWrapper) Method() string {
	method, ok := r.dict["method"].(string)
	if !ok {
		return ""
	}
	return method
}

func (r AcceptFileRuleWrapper) Auth() *AcceptFileRuleAuth {
	authDict, ok := r.dict["auth"].(map[string]any)
	if !ok {
		return nil
	}
	auth := &AcceptFileRuleAuth{}
	if scheme, ok := authDict["scheme"].(string); ok {
		auth.Scheme = scheme
	}
	if username, ok := authDict["username"].(string); ok {
		auth.Username = username
	}
	if password, ok := authDict["password"].(string); ok {
		auth.Password = password
	}
	if token, ok := authDict["token"].(string); ok {
		auth.Token = token
	}
	return auth
}

func (r AcceptFileRuleWrapper) Headers() ResolverMap {
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
	Auth    *AcceptFileRuleAuth `json:"auth,omitempty"`
	Headers map[string]string   `json:"headers,omitempty"`
}

type AcceptFileRuleAuth struct {
	Scheme   string `json:"scheme"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
}
