package common

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Integration string

const (
	// NOTE: these MUST match up with values from the server side enum
	IntegrationCustom     Integration = ""
	IntegrationGit        Integration = "git"
	IntegrationGithub     Integration = "github"
	IntegrationSlack      Integration = "slack"
	IntegrationJira       Integration = "jira"
	IntegrationGitlab     Integration = "gitlab"
	IntegrationAws        Integration = "AWS"
	IntegrationSonarqube  Integration = "sonarqube"
	IntegrationBitbucket  Integration = "bitbucket"
	IntegrationPrometheus Integration = "prometheus"
)

var subtypes = map[Integration][]string{
	IntegrationJira:      {"bearer"},
	IntegrationBitbucket: {"basic"},
}

func (i Integration) String() string {
	return string(i)
}

func (i Integration) Validate() error {

	for _, integration := range ValidIntegrations() {
		if i == integration {
			return nil
		}
	}

	return fmt.Errorf("invalid integration %s", i)
}

func ParseIntegration(s string) (Integration, error) {
	i := Integration(s)
	if err := i.Validate(); err != nil {
		return "", err
	}
	return i, nil
}

func ValidIntegrations() []Integration {
	return []Integration{IntegrationGithub, IntegrationJira, IntegrationGitlab, IntegrationBitbucket, IntegrationSonarqube, IntegrationPrometheus}
}

type IntegrationInfo struct {
	Integration    Integration
	Subtype        string
	Alias          string
	AcceptFilePath string
}

func (ii IntegrationInfo) AcceptFile(localhostBase string) (string, error) {

	// load/locate the accept file then fix it up to have
	// an entry to talk to the axon HTTP server itself
	// which we use for status, etc
	alias := ii.Alias
	if len(alias) == 0 {
		alias = "default"
	}

	content, err := ii.getAcceptFileContents()
	if err != nil {
		return "", err
	}

	rawFile := []byte(content)
	h := sha256.New()
	h.Write(rawFile)
	expectedPath := path.Join(
		os.TempDir(),
		"accept-files",
		ii.Integration.String(),
		alias,
		hex.EncodeToString(h.Sum(nil)),
		"accept.json",
	)

	dict := map[string]interface{}{}
	err = json.Unmarshal(rawFile, &dict)
	if err != nil {
		return "", err
	}

	// we add a section like this to allow the server side
	// to hit the axon agent via an HTTP bridge
	// "private": [
	// 	{
	// 	  "method": "any",
	// 	  "path": "/__axon/*",
	// 	  "origin": "http://localhost"
	// 	}
	//   ]

	entries, ok := dict["private"].([]interface{})
	if !ok {
		entries = []interface{}{}
		dict["private"] = entries
	}

	entry := map[string]string{
		"method": "any",
		"path":   "/__axon/*",
		"origin": localhostBase,
	}
	dict["private"] = append([]interface{}{entry}, entries...)

	if _, ok := dict["public"]; !ok {
		dict["public"] = []interface{}{}
	}

	json, err := json.Marshal(dict)
	if err != nil {
		return "", err
	}
	err = os.MkdirAll(path.Dir(expectedPath), os.ModeDir|os.ModePerm)
	if err != nil {
		return "", err
	}
	err = os.WriteFile(expectedPath, json, os.ModePerm)

	return expectedPath, err
}

type ValueResolver func() string

func StringValueResolver(value string) ValueResolver {
	return func() string {
		return value
	}
}

type ResolverMap map[string]ValueResolver

func NewResolverMapFromMap(m map[string]string) ResolverMap {
	rm := make(ResolverMap, len(m))
	for key, value := range m {
		rm[key] = StringValueResolver(value)
	}
	return rm
}

func (rm ResolverMap) Resolve() map[string]string {
	resolved := make(map[string]string, len(rm))
	for key, resolver := range rm {
		resolved[key] = resolver()
	}
	return resolved
}

func (rm ResolverMap) ResolveKey(key string) string {
	resolver, ok := rm[key]
	if !ok {
		return ""
	}
	return resolver()
}

func EnvValueResolver(envVar string, defaultValue string, capture bool) ValueResolver {

	captured := os.ExpandEnv(envVar)
	return func() string {
		if capture {
			return captured
		}
		if val := os.ExpandEnv(envVar); val != "" {
			return val
		}

		return defaultValue
	}
}

func (ii IntegrationInfo) RewriteOrigins(acceptFilePath string, writer func(string, ResolverMap) string) (*AcceptFileInfo, error) {

	info, err := newAcceptFileInfo(acceptFilePath, writer)
	if err != nil {
		return nil, err
	}
	_, err = info.Rewrite()
	if err != nil {
		return nil, err
	}
	return info, nil
}

type AcceptFileInfo struct {
	OriginalPath   string
	RewrittenPath  string
	Content        string
	rawContent     []byte
	Routes         []AcceptFileRoute
	originRewriter func(uri string, headers ResolverMap) string
}

var IgnoreHosts = []string{
	"localhost",
	"127.0.0.1",
}

func newAcceptFileInfo(acceptFilePath string, originRewriter func(string, ResolverMap) string) (*AcceptFileInfo, error) {
	stat, err := os.Stat(acceptFilePath)
	if err != nil {
		return nil, err
	}

	if stat.IsDir() {
		return nil, fmt.Errorf("accept file path %q is a directory, expected a file", acceptFilePath)
	}

	info := &AcceptFileInfo{
		OriginalPath:   acceptFilePath,
		originRewriter: originRewriter,
	}

	info.rawContent, err = os.ReadFile(acceptFilePath)
	if err != nil {
		return nil, err
	}

	return info, nil
}

func (afi *AcceptFileInfo) isIgnoredHost(host string) bool {
	for _, ignoreHost := range IgnoreHosts {
		if strings.HasPrefix(host, ignoreHost) {
			return true
		}
	}
	return false
}

func (afi *AcceptFileInfo) Rewrite() (string, error) {

	if afi.Content == "" {
		dict := map[string]interface{}{}
		err := json.Unmarshal(afi.rawContent, &dict)
		if err != nil {
			return "", err
		}

		entries, ok := dict["private"].([]interface{})
		if !ok {
			return "", nil
		}

		for _, entry := range entries {
			values := entry.(map[string]any)
			rawOrigin, ok := values["origin"].(string)
			if !ok {
				continue
			}

			origin := os.ExpandEnv(rawOrigin)

			parsed, err := url.Parse(origin)
			if err != nil {
				return "", fmt.Errorf("failed to parse origin %q: %w", origin, err)
			}

			if afi.isIgnoredHost(parsed.Host) {
				continue
			}

			if parsed.Scheme == "" {
				parsed.Scheme = "https"
				origin = parsed.String()
			}

			// Extract headers if present
			var headers ResolverMap
			if headersInterface, hasHeaders := values["headers"]; hasHeaders {
				if headersMap, ok := headersInterface.(map[string]interface{}); ok {
					headers = make(ResolverMap, len(headersMap))
					for k, v := range headersMap {
						if strVal, ok := v.(string); ok {
							// Resolve environment variables in header values
							headers[k] = EnvValueResolver(strVal, "", true)
						}
					}
				}
			}

			// rewrite the origin to use the writer function
			newOrigin := afi.originRewriter(origin, headers)
			if newOrigin != "" {
				values["origin"] = newOrigin
			}
			afi.Routes = append(afi.Routes, AcceptFileRoute{
				ResolvedOrigin: origin,
				ProxyOrigin:    newOrigin,
				Headers:        headers,
			})
		}

		json, err := json.Marshal(dict)
		if err != nil {
			return "", err
		}
		afi.Content = string(json)

		stat, err := os.Stat(afi.OriginalPath)

		if err != nil {
			return "", err
		}
		newFilePath := path.Join(
			os.TempDir(),
			"accept-files-written",
			fmt.Sprintf("rewrite.%v.%v", time.Now().UnixMilli(), stat.Name()),
		)
		err = os.MkdirAll(path.Dir(newFilePath), os.ModeDir|os.ModePerm)
		if err != nil {
			return "", err
		}

		err = os.WriteFile(newFilePath, json, os.ModePerm)
		if err != nil {
			return "", err
		}
		afi.RewrittenPath = newFilePath
	}
	return afi.Content, nil
}

type AcceptFileRoute struct {
	ResolvedOrigin string
	ProxyOrigin    string
	Headers        ResolverMap
}

func (ii IntegrationInfo) getAcceptFileContents() (string, error) {
	if ii.AcceptFilePath != "" {

		// load the file and add the stanza for axon
		rf, err := os.ReadFile(ii.AcceptFilePath)
		if err != nil {
			return "", err
		}
		return string(rf), nil
	}

	// look for an integration file
	integrationAcceptFile, err := ii.getIntegrationAcceptFile()
	if err != nil || integrationAcceptFile != "" {
		return integrationAcceptFile, err
	}

	// we default to an empty accept file
	return "{}", nil
}

func (ii IntegrationInfo) ValidateSubtype() (string, error) {
	if ii.Subtype == "" {
		return "", nil
	}

	allowedSubtypes, ok := subtypes[ii.Integration]
	if !ok {
		return "", fmt.Errorf("integration %s does not support subtypes", ii.Integration)
	}

	for _, subtype := range allowedSubtypes {
		if subtype == ii.Subtype {
			return strings.ToLower(ii.Subtype), nil
		}
	}

	return "", fmt.Errorf("integration %s does not support subtype %s, allowed values are: %v", ii.Integration, ii.Subtype, allowedSubtypes)
}

func (ii IntegrationInfo) GetValidationConfig() *ValidationConfig {
	if ii.Integration == IntegrationCustom {
		return nil
	}

	selector := fmt.Sprintf("config.%s.json", ii.Integration) // config.<integration>.<subtype>
	contents, err := ii.getIntegrationFileContents(selector)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Println("Error reading integration file:", err)
		}
		return nil
	}

	var config IntegrationConfig
	if err := json.Unmarshal([]byte(contents), &config); err != nil {
		fmt.Println("Error unmarshalling integration file:", err)
		return nil
	}
	validationsBySubtype := make(map[string]ValidationConfig)
	for _, v := range config.Validation {
		validationsBySubtype[v.Subtype] = v
	}
	if v, ok := validationsBySubtype[ii.Subtype]; ok {
		return &v
	}
	if v, ok := validationsBySubtype[""]; ok {
		return &v
	}
	return nil
}

type IntegrationConfig struct {
	Validation []ValidationConfig `json:"validation"`
}
type ValidationConfig struct {
	Subtype string `json:"subtype,omitempty"`
	URL     string `json:"url"`
	Method  string `json:"method,omitempty"`
	Auth    Auth   `json:"auth"`
}

type Auth struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type Config struct {
	Validation ValidationConfig `json:"$validation"`
}

func (ii IntegrationInfo) getIntegrationFileContents(fileName string) (string, error) {
	acceptFileDir := os.Getenv("ACCEPTFILE_DIR")

	if acceptFileDir == "" {
		acceptFileDir = "server/snykbroker/accept_files"
	}

	fullPath := path.Join(acceptFileDir, fileName)
	contents, err := os.ReadFile(fullPath)

	if err != nil {
		return "", err
	}
	return string(contents), nil
}

func (ii IntegrationInfo) getIntegrationAcceptFile() (string, error) {

	if ii.Integration == IntegrationCustom {
		return "", nil
	}

	selector := ii.Integration.String()
	if ii.Subtype != "" {
		subtype, err := ii.ValidateSubtype()
		if err != nil {
			return "", err
		}
		selector = fmt.Sprintf("%s.%s", selector, subtype)
	}

	fileName := fmt.Sprintf("accept.%s.json", selector)

	strContent, err := ii.getIntegrationFileContents(fileName)
	if err != nil {
		return "", err
	}
	if err := ii.ensureAcceptFileVars(strContent); err != nil {
		return "", err
	}
	return strContent, nil
}

var reContentVars = regexp.MustCompile(`\$\{(.*?)\}`)

func (ii IntegrationInfo) ensureAcceptFileVars(content string) error {
	varMatch := reContentVars.FindAllStringSubmatch(content, -1)

	envVars := []string{}

	// sort these so they have a stable order

	for _, match := range varMatch {
		envVars = append(envVars, match[1])
	}

	sort.Strings(envVars)

	for _, envVar := range envVars {
		if os.Getenv(envVar) == "" && os.Getenv(envVar+"_POOL") == "" {
			return fmt.Errorf("missing required environment variable %q for integration %s", envVar, ii.Integration.String())
		}
	}
	return nil
}
