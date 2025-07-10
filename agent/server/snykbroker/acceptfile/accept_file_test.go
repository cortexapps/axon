package acceptfile

import (
	"fmt"
	"os"
	"strings"
	"testing"

	axonConfig "github.com/cortexapps/axon/config"
	"go.uber.org/zap"

	"github.com/stretchr/testify/require"
)

func TestEmptyAcceptFile(t *testing.T) {

	acceptFiles := []string{
		"{}",
		`{"private": [], "public": []}`,
		`{"private": []}`,
		`{"public": []}`,
	}

	for _, acceptFileContents := range acceptFiles {
		t.Run(acceptFileContents, func(t *testing.T) {
			cfg := axonConfig.NewAgentEnvConfig()
			cfg.HttpServerPort = 9999
			acceptFile, err := NewAcceptFile([]byte(acceptFileContents), cfg, nil)
			require.NoError(t, err)
			contents, err := acceptFile.Render(zap.NewNop())
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("{\"private\":[{\"method\":\"any\",\"origin\":\"%s\",\"path\":\"/__axon/*\"}],\"public\":[]}", cfg.HttpBaseUrl()), string(contents))
		})
	}
}

func TestAcceptFileValidate(t *testing.T) {

	cfg := axonConfig.NewAgentEnvConfig()

	files := []struct {
		content string
		valid   bool
		envVars map[string]string
	}{
		{
			content: `{"private": [], "public": []}`,
			valid:   true,
			envVars: nil,
		},
		{
			content: `{"private": [
				{"method": "GET", "origin": "${API}", "path": "/*"}
			]}`,
			valid:   true,
			envVars: map[string]string{"API": "value"},
		},
		{
			content: `{"private": [
				{"method": "GET", "origin": "${API}", "path": "/*"}
			]}`,
			valid:   false,
			envVars: nil,
		},
		{
			content: `{"private": [
				{"method": "GET", "origin": "${plugin:API}", "path": "/*"}
			]}`,
			valid:   true,
			envVars: nil,
		},
		{
			content: `{"private": [
				{"method": "GET", "origin": "${env:API}", "path": "/*"}
			]}`,
			valid:   false,
			envVars: nil,
		},
		{
			content: `{"vars": ["${env:API}", "${OTHER}"], "private": []}`,
			valid:   true,
			envVars: map[string]string{"API": "value", "OTHER": "othervalue"},
		},
	}

	for _, file := range files {
		t.Run(file.content, func(t *testing.T) {
			if file.envVars != nil {
				for k, v := range file.envVars {
					os.Setenv(k, v)
				}
				t.Cleanup(func() {
					for k := range file.envVars {
						os.Unsetenv(k)
					}
				})
			}
			_, err := NewAcceptFile([]byte(file.content), cfg, nil)
			if file.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				return
			}

		})
	}

}

func TestRenderEnvVars(t *testing.T) {

	vars := map[string]string{
		"API":    "value",
		"OTHER":  "othervalue",
		"plugin": "nope",
	}

	for k, v := range vars {
		os.Setenv(k, v)
	}
	t.Cleanup(func() {
		for k := range vars {
			os.Unsetenv(k)
		}
	})

	cfg := axonConfig.NewAgentEnvConfig()

	content := `{
		"$vars":["${env:API}", "${OTHER}", "${plugin:foo}", "${OTHER}"], "private": []}`

	af, err := NewAcceptFile([]byte(content), cfg, nil)
	require.NoError(t, err)
	rendered, err := af.Render(zap.NewNop())
	require.NoError(t, err)
	expected := `{"$vars":["${API}","${OTHER}","{{plugin:foo}}","${OTHER}"],"private":[{"method":"any","origin":"http://localhost:80","path":"/__axon/*"}],"public":[]}`
	require.Equal(t, expected, string(rendered), "Rendered accept file does not match expected output")
}

func TestExtraRenderSteps(t *testing.T) {
	acceptFileContents := `{
		
		"private": [
			{"method": "GET", "origin": "http://localhost:9999", "path": "/private/*"}
		]
	}`
	cfg := axonConfig.NewAgentEnvConfig()
	cfg.HttpServerPort = 9999
	logger := zap.NewNop()
	acceptFile, err := NewAcceptFile([]byte(acceptFileContents), cfg, nil)
	require.NoError(t, err)

	rendered, err := acceptFile.Render(logger, func(renderContext RenderContext) error {

		for _, entry := range renderContext.AcceptFile.PrivateRules() {
			if !strings.Contains(entry.Path(), "axon") {
				entry.SetOrigin("http://localhost:8888")
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, rendered)
	expected := `{"private":[{"method":"any","origin":"http://localhost:9999","path":"/__axon/*"},{"method":"GET","origin":"http://localhost:8888","path":"/private/*"}],"public":[]}`
	require.Equal(t, expected, string(rendered), "Rendered accept file does not match expected output")
}

func TestPreProcessContent(t *testing.T) {
	content := `{
		"$vars":[
			"${env:API}",
			"${OTHER}",
			"${plugin:foo}",
			"${OTHER}"
		]
	}`

	expected := `{
		"$vars":[
			"${API}",
			"${OTHER}",
			"{{plugin:foo}}",
			"${OTHER}"
		]
	}`

	processed, err := preProcessContent([]byte(content))
	require.NoError(t, err)
	require.Equal(t, expected, string(processed), "Processed content does not match expected output")
}
