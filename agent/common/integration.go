package common

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

type Integration string

const (
	// NOTE: these MUST match up with values from the server side enum
	IntegrationCustom    Integration = ""
	IntegrationGithub    Integration = "github"
	IntegrationSlack     Integration = "slack"
	IntegrationJira      Integration = "jira"
	IntegrationGitlab    Integration = "gitlab"
	IntegrationAws       Integration = "AWS"
	IntegrationSonarqube Integration = "sonarqube"
)

var subtypes = map[Integration][]string{
	IntegrationGithub: {"app"},
	IntegrationJira:   {"bearer"},
}

func (i Integration) String() string {
	return string(i)
}

func (i Integration) Validate() error {
	switch i {
	case IntegrationGithub, IntegrationSlack, IntegrationJira, IntegrationGitlab, IntegrationAws, IntegrationSonarqube:
		return nil
	default:
		return fmt.Errorf("invalid integration %s", i)
	}
}

func ParseIntegration(s string) (Integration, error) {
	i := Integration(s)
	if err := i.Validate(); err != nil {
		return "", err
	}
	return i, nil
}

func ValidIntegrations() []Integration {
	return []Integration{IntegrationGithub, IntegrationSlack, IntegrationJira, IntegrationGitlab, IntegrationAws, IntegrationSonarqube}
}

type IntegrationInfo struct {
	Integration    Integration
	Subtype        string
	Alias          string
	AcceptFilePath string
}

func (ii IntegrationInfo) AcceptFile() (string, error) {

	// load/locate the accept file then fix it up to have
	// an entry to talk to the neuroh HTTP server itself
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
		"origin": "http://localhost",
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

	acceptFileDir := os.Getenv("ACCEPT_FILE_DIR")

	if acceptFileDir == "" {
		acceptFileDir = "server/snykbroker/accept_files"
	}

	fullPath := path.Join(acceptFileDir, fileName)
	contents, err := os.ReadFile(fullPath)

	if err != nil {
		return "", err
	}

	strContent := string(contents)
	if err := ii.ensureAcceptFileVars(strContent); err != nil {
		return "", err
	}
	return strContent, nil
}

var reContentVars = regexp.MustCompile(`\$\{(.*?)\}`)

func (ii IntegrationInfo) ensureAcceptFileVars(content string) error {
	varMatch := reContentVars.FindAllStringSubmatch(content, -1)

	for _, match := range varMatch {
		envVar := match[1]
		if os.Getenv(envVar) == "" {
			return fmt.Errorf("missing required environment variable %q for integration %s", envVar, ii.Integration.String())
		}
	}
	return nil
}
