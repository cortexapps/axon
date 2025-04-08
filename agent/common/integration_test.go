package common

import (
	"fmt"
	"os"
	"path"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEmptyAcceptFile(t *testing.T) {

	acceptFileContents := "{}"
	acceptFilePath := writeTempFile(t, acceptFileContents)

	info := IntegrationInfo{
		Integration:    IntegrationGithub,
		Alias:          fmt.Sprintf("%v", time.Now().UnixMilli()),
		AcceptFilePath: acceptFilePath,
	}

	defer func() {
		os.Remove(info.AcceptFilePath)
	}()

	resultPath, err := info.AcceptFile()
	require.NoError(t, err)

	contents, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	require.Equal(t, "{\"private\":[{\"method\":\"any\",\"origin\":\"http://localhost\",\"path\":\"/__axon/*\"}],\"public\":[]}", string(contents))

}

func TestGithubDefaultAcceptFile(t *testing.T) {

	os.Setenv("GITHUB_TOKEN", "the-github-token")
	os.Setenv("GITHUB_API", "foo.github.com")
	os.Setenv("GITHUB_GRAPHQL", "foo.github.com/graphql")

	setAcceptFileDir(t)

	info := IntegrationInfo{
		Integration: IntegrationGithub,
		Alias:       fmt.Sprintf("%v", time.Now().UnixMilli()),
	}

	resultPath, err := info.AcceptFile()
	require.NoError(t, err)
	_, err = os.Stat(resultPath)
	require.NoError(t, err)
}

func TestGithubDefaultAcceptFileSubtypeInvalid(t *testing.T) {

	os.Setenv("GITHUB_TOKEN", "the-github-token")
	os.Setenv("GITHUB_API", "foo.github.com")
	os.Setenv("GITHUB_GRAPHQL", "foo.github.com/graphql")

	setAcceptFileDir(t)

	info := IntegrationInfo{
		Integration: IntegrationGithub,
		Subtype:     "xyz",
		Alias:       fmt.Sprintf("%v", time.Now().UnixMilli()),
	}

	_, err := info.AcceptFile()
	require.Error(t, err)
}

func TestExistingAcceptFile(t *testing.T) {

	acceptFileContents := `
	{
		"public": [
		{
			"method": "any",
			"path": "/*"
		}
		],
		"private": [
		{
			"method": "any",
			"path": "/*",
			"origin": "http://python-server"
		}
		]
	}	
	`
	acceptFilePath := writeTempFile(t, acceptFileContents)

	info := IntegrationInfo{
		Integration:    IntegrationGithub,
		AcceptFilePath: acceptFilePath,
	}

	defer func() {
		os.Remove(info.AcceptFilePath)
	}()

	resultPath, err := info.AcceptFile()
	require.NoError(t, err)

	contents, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	require.Equal(t, `{"private":[{"method":"any","origin":"http://localhost","path":"/__axon/*"},{"method":"any","origin":"http://python-server","path":"/*"}],"public":[{"method":"any","path":"/*"}]}`, string(contents))

}

func setAcceptFileDir(t *testing.T) {
	pwd, err := os.Getwd()
	require.NoError(t, err)
	acceptFileDir := path.Join(pwd, "..", "server", "snykbroker", "accept_files")
	os.Setenv("ACCEPT_FILE_DIR", acceptFileDir)
}

func loadAcceptFile(t *testing.T, integration Integration) (string, error) {
	setAcceptFileDir(t)
	ii := IntegrationInfo{
		Integration: integration,
	}
	return ii.AcceptFile()
}
func init() {
	setAcceptFileDir(&testing.T{})
}

func TestLoadIntegrationAcceptFileSuccess(t *testing.T) {

	os.Setenv("GITHUB_TOKEN", "the-github-token")
	os.Setenv("GITHUB_API", "foo.github.com")
	os.Setenv("GITHUB_GRAPHQL", "foo.github.com/graphql")

	acceptFile, err := loadAcceptFile(t, IntegrationGithub)
	require.NoError(t, err)
	require.NotEmpty(t, acceptFile)
}

func TestLoadIntegrationAcceptFileMissingVars(t *testing.T) {
	os.Setenv("GITHUB_TOKEN", "")
	os.Setenv("GITHUB_API", "")

	acceptFile, err := loadAcceptFile(t, IntegrationGithub)
	require.Error(t, err)
	require.Contains(t, err.Error(), "GITHUB_API")
	require.Empty(t, acceptFile)
}

func TestLoadIntegrationAcceptFilePoolVars(t *testing.T) {
	os.Setenv("GITHUB_TOKEN_POOL", "its-mah-token,its-mah-other-token")
	os.Setenv("GITHUB_API", "github.com")
	os.Setenv("GITHUB_GRAPHQL", "github.com/graphql")

	acceptFile, err := loadAcceptFile(t, IntegrationGithub)
	require.NoError(t, err)
	contents, err := os.ReadFile(acceptFile)
	require.NoError(t, err)
	require.Contains(t, string(contents), "GITHUB_TOKEN")
	require.NotContains(t, string(contents), "GITHUB_TOKEN_POOL")
}

func TestLoadValidationParams(t *testing.T) {
	ii := IntegrationInfo{
		Integration: IntegrationGithub,
	}

	validationParams := ii.GetValidationConfig()
	require.NotNil(t, validationParams)
	require.Equal(t, "$GITHUB_API/user", validationParams.URL)
}
func TestLoadValidationParamsSubtype(t *testing.T) {
	ii := IntegrationInfo{
		Integration: IntegrationGithub,
		Subtype:     "app",
	}

	validationParams := ii.GetValidationConfig()
	require.NotNil(t, validationParams)
	require.Equal(t, "$GITHUB_API/user", validationParams.URL)
}

func TestLoadValidationParamsJiraSubtype(t *testing.T) {

	ii := IntegrationInfo{
		Integration: IntegrationJira,
	}

	validationParams := ii.GetValidationConfig()
	require.NotNil(t, validationParams)
	require.Equal(t, "basic", validationParams.Auth.Type)
	require.Equal(t, "$JIRA_USERNAME:$JIRA_PASSWORD", validationParams.Auth.Value)

	ii = IntegrationInfo{
		Integration: IntegrationJira,
		Subtype:     "bearer",
	}

	validationParams = ii.GetValidationConfig()
	require.NotNil(t, validationParams)
	require.Equal(t, "header", validationParams.Auth.Type)
	require.Equal(t, "Bearer $JIRA_TOKEN", validationParams.Auth.Value)
}

func TestLoadValidationParamsBitbucketSubtype(t *testing.T) {

	ii := IntegrationInfo{
		Integration: IntegrationBitbucket,
		Subtype:     "basic",
	}

	validationParams := ii.GetValidationConfig()
	require.NotNil(t, validationParams)
	require.Equal(t, "basic", validationParams.Auth.Type)
	require.Equal(t, "$BITBUCKET_USERNAME:$BITBUCKET_PASSWORD", validationParams.Auth.Value)

	ii = IntegrationInfo{
		Integration: IntegrationBitbucket,
	}

	validationParams = ii.GetValidationConfig()
	require.NotNil(t, validationParams)
	require.Equal(t, "header", validationParams.Auth.Type)
	require.Equal(t, "Bearer $BITBUCKET_TOKEN", validationParams.Auth.Value)
}

func writeTempFile(t *testing.T, contents string) string {
	f, err := os.CreateTemp(t.TempDir(), "accept.*.json")
	require.NoError(t, err)
	defer f.Close()

	_, err = f.WriteString(contents)
	require.NoError(t, err)
	return f.Name()
}
