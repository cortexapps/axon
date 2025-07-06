package common

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
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
	return []Integration{IntegrationCustom, IntegrationGithub, IntegrationJira, IntegrationGitlab, IntegrationBitbucket, IntegrationSonarqube, IntegrationPrometheus}
}

type IntegrationInfo struct {
	Integration    Integration
	Subtype        string
	Alias          string
	AcceptFilePath string
}

func (ii IntegrationInfo) Validate() error {
	if err := ii.Integration.Validate(); err != nil {
		return err
	}
	if _, err := ii.ValidateSubtype(); err != nil {
		return err
	}
	return nil
}

func (ii IntegrationInfo) ToAcceptFile(cfg config.AgentConfig) (*acceptfile.AcceptFile, error) {

	if err := ii.Validate(); err != nil {
		return nil, err
	}

	content, err := ii.getAcceptFileContents()
	if err != nil {
		return nil, err
	}
	return acceptfile.NewAcceptFile([]byte(content), cfg)
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
	return strContent, nil
}
