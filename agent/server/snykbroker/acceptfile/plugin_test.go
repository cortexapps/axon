package acceptfile

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestEnvVarTypes(t *testing.T) {
	content := `	
		${NORMAL_ENV_VAR}
		${plugin:PLUGIN_NAME}
		${env:ENV_VAR} 	
	`

	os.Setenv("NORMAL_ENV_VAR", "normal_value")
	os.Setenv("PLUGIN_NAME", "INVALID")
	os.Setenv("ENV_VAR", "env_value")

	t.Cleanup(func() {
		os.Unsetenv("NORMAL_ENV_VAR")
		os.Unsetenv("PLUGIN_NAME")
		os.Unsetenv("ENV_VAR")
	})

	fileVars := findFileVars(content)

	expectedVars := []fileVar{
		{Name: "NORMAL_ENV_VAR", Type: VarTypeEnv, Original: "${NORMAL_ENV_VAR}"},
		{Name: "PLUGIN_NAME", Type: VarTypePlugin, Original: "${plugin:PLUGIN_NAME}"},
		{Name: "ENV_VAR", Type: VarTypeEnv, Original: "${env:ENV_VAR}"},
	}

	require.ElementsMatch(t, expectedVars, fileVars, "File vars do not match expected values")

}

func TestParsePluginVar(t *testing.T) {
	content := `${plugin:my-plugin}`
	item := parseEnvType(content)
	require.Equal(t, "my-plugin", item.Name, "Plugin name should match")
	require.Equal(t, VarTypePlugin, item.Type, "Variable type should be VarTypePlugin")
}

func TestParseReplacedFormat(t *testing.T) {
	content := `{{plugin:my-plugin}}`
	item := parseInterpolation(content)
	require.Equal(t, "my-plugin", item.Name, "Plugin name should match")
	require.Equal(t, VarTypePlugin, item.Type, "Variable type should be VarTypePlugin")
}

func TestCreatePluginResolver(t *testing.T) {
	// Assuming the plugin.sh is in the same directory as the test file

	logger := zap.NewNop()

	// Create a plugin resolver
	resolver := CreateResolver("{{plugin:plugin.sh}}", logger, []string{"."})

	// Execute the resolver and get the output
	output := resolver.Resolve()
	require.NotEmpty(t, output, "Output should not be empty")
	require.Contains(t, output, "HOME="+os.Getenv("HOME"), "Output should contain $HOME, but was: "+output)
}

func TestPluginNotPresent(t *testing.T) {

	require.Panics(t, func() {
		NewPlugin("test-plugin", "bad-path", zap.NewNop())
	})

}

func TestPluginExecution(t *testing.T) {
	// Assuming the plugin.sh is in the same directory as the test file
	pluginPath := "./plugin.sh"
	plugin := NewPlugin("test-plugin", pluginPath, zap.NewNop())
	output, err := plugin.Execute()
	require.NoError(t, err, "Plugin execution should not return an error")
	require.NotEmpty(t, output, "Output should not be empty")
	require.Contains(t, output, "HOME="+os.Getenv("HOME"), "Output should contain $HOME, but was: "+output)
}

func TestPluginExecutionFail(t *testing.T) {
	// Assuming the plugin.sh is in the same directory as the test file
	pluginPath := "./plugin_fail.sh"
	plugin := NewPlugin("test-plugin-fail", pluginPath, zap.NewNop())
	output, err := plugin.Execute()
	require.Error(t, err, "Plugin execution should not return an error")
	require.Contains(t, err.Error(), "exit status 1", "Error message should indicate failure to run")
	require.Empty(t, output, "Output should not be empty")
}

func TestFindPlugin(t *testing.T) {
	// Assuming the plugin.sh is in the same directory as the test file
	pluginFile := "plugin.sh"
	logger := zap.NewNop()

	// Test finding an existing plugin
	plugin, err := FindPlugin(pluginFile, []string{"."}, logger)
	require.NoError(t, err, "Should find the plugin")
	require.Equal(t, pluginFile, plugin.Name)
	require.Equal(t, "./plugin.sh", plugin.FullPath)

	// Test finding a non-existing plugin
	_, err = FindPlugin("nonexistent.sh", []string{"."}, logger)
	require.Error(t, err, "Should not find a non-existing plugin")

	// Test finding non-executable plugin
	pluginFile = "plugin_test.go"
	_, err = os.Stat(pluginFile)
	require.NoError(t, err, "Should be able to stat the plugin file")
	_, err = FindPlugin(pluginFile, []string{"."}, logger)
	require.Error(t, err, "Should not find a non-executable plugin")

}
