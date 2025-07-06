package acceptfile

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// preProcessContent processes the content to replace plugin placeholders
// for simplicity the vars in the raw accept file are of the form
// "${plugin:foo}" and we replace them with "{{plugin:foo}}" to avoid
// conflicts with env variables os we can use the system env expansion
func preProcessContent(content []byte) ([]byte, error) {
	// Change all "${plugin:foo}" to "{{ plugin:foo }}"
	rePlugin := regexp.MustCompile(`\$\{plugin:([^}]+)\}`)
	content = rePlugin.ReplaceAll(content, []byte("{{plugin:$1}}"))

	// Change all "${env:FOO}" to "${FOO}" (unchanged)
	reEnv := regexp.MustCompile(`\$\{env:([^}]+)\}`)
	content = reEnv.ReplaceAll(content, []byte("${$1}"))

	return content, nil
}

// CreateResolver takes content and resolves it to a function that will do any
// interpolation and plugin execution when called.
// It expands environment variables and finds plugins in the provided directories.
// Then for each execution we loop the plugins and replace the plugin placeholder content
func CreateResolver(value string, logger *zap.Logger, pluginDirs []string) ValueResolver {

	// here we expand all the env vars and then
	// look for plugins and execute those.
	content := os.ExpandEnv(value)
	plugins := findPlugins(content, pluginDirs, logger)

	return ValueResolver{
		Key: value,
		Resolve: func() string {

			execContent := content
			for _, plugin := range plugins {
				// replace the plugin variable with the output of the plugin
				pluginOutput, err := plugin.Plugin.Execute()
				if err != nil {
					logger.Error("failed to execute plugin", zap.String("plugin", plugin.Plugin.FullPath), zap.Error(err))
					continue
				}
				execContent = strings.ReplaceAll(execContent, plugin.Content, strings.Trim(pluginOutput, "\n"))
			}
			return execContent
		},
	}
}

type PluginResult struct {
	Plugin
	Content string
}

// findPlugins finds all plugin invocations in the content and returns a list of PluginResults
// each of which represents a plugin that was found and its content in the accept file.
func findPlugins(content string, pluginDirs []string, logger *zap.Logger) []PluginResult {

	pluginStrings := reInterpolation.FindAllString(content, -1)

	seen := map[string]bool{}

	plugins := make([]PluginResult, 0, len(pluginStrings))
	for _, pluginString := range pluginStrings {

		if seen[pluginString] {
			continue
		}

		result := parseInterpolation(pluginString)
		if result.Type != VarTypePlugin {
			logger.Panic("expected plugin type", zap.String("plugin", result.Name), zap.String("content", content))
		}
		plugin, err := FindPlugin(result.Name, pluginDirs, logger)
		if err != nil {
			logger.Panic(
				fmt.Sprintf("failed to find plugin from %q", result.Name),
				zap.String("plugin", result.Name),
				zap.String("workingDir", os.Getenv("PWD")),
				zap.Error(err))
		}
		plugins = append(plugins, PluginResult{
			Plugin:  plugin,
			Content: pluginString,
		})
		seen[result.Name] = true
	}

	return plugins
}

type ValueResolver struct {
	Resolve func() string
	Key     string
}

func StringValueResolver(value string) ValueResolver {
	return ValueResolver{
		Resolve: func() string {
			return value
		},
		Key: value,
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

func (rm ResolverMap) ToStringMap() map[string]string {
	resolved := make(map[string]string, len(rm))
	for key, resolver := range rm {
		resolved[key] = resolver.Resolve()
	}
	return resolved
}

func (rm ResolverMap) Resolve(key string) string {
	resolver, ok := rm[key]
	if !ok {
		return ""
	}
	return resolver.Resolve()
}

func (rm ResolverMap) ResolverKey(key string) string {
	resolver, ok := rm[key]
	if !ok {
		return ""

	}
	return resolver.Key
}

// Parsing code for extracting environment variables and plugin invocations from content
// Examples: `${env:API}`, `${plugin:my-plugin}`, `${API}`
var reContentVars = regexp.MustCompile(`(?m)\$\{([^:}]+):([^}]+)\}|\$\{([^}]+)\}`)

const VAR_TYPE_INDEX = 1
const VAR_NAME_INDEX = 2
const VAR_NAME_ONLY_INDEX = 3

// define an enum of variable types
type varType int

const (
	VarTypeEnv varType = iota
	VarTypePlugin
)

type fileVar struct {
	Name     string
	Type     varType
	Original string
}

func (vt varType) String() string {
	switch vt {
	case VarTypeEnv:
		return "Env"
	case VarTypePlugin:
		return "Plugin"
	default:
		return "Unknown"
	}
}

// Parser for interpolation in the accept file
// This is used to parse the `{{plugin:my-plugin}}` format in the accept file.
var reInterpolation = regexp.MustCompile(`\{\{([^}]+)\}\}`)

func parseInterpolation(content string) fileVar {
	match := reInterpolation.FindStringSubmatch(content)
	if match == nil {
		panic(fmt.Sprintf("invalid interpolation format: %q", content))
	}
	found := strings.Trim(match[1], " ")
	parts := strings.Split(found, ":")
	return fileVar{
		Name:     strings.Trim(parts[1], " "),
		Type:     VarTypePlugin,
		Original: match[0],
	}
}

func parseEnvType(content string) fileVar {

	match := reContentVars.FindStringSubmatch(content)
	if len(match) < 4 {
		panic(fmt.Sprintf("invalid env var format %q", content))
	}
	varTypeName := match[VAR_TYPE_INDEX]
	value := match[VAR_NAME_INDEX]
	if value == "" {
		value = match[VAR_NAME_ONLY_INDEX]
	}

	switch varTypeName {
	case "env", "":
		return fileVar{value, VarTypeEnv, content}
	case "plugin":
		return fileVar{value, VarTypePlugin, content}
	default:
		panic(fmt.Sprintf("unknown env var type %q", content))
	}
}

func findFileVars(content string) []fileVar {
	varMatch := reContentVars.FindAllStringSubmatch(content, -1)

	envVars := []fileVar{}

	// sort these so they have a stable order
	for _, match := range varMatch {
		fv := parseEnvType(match[0])
		envVars = append(envVars, fv)
	}

	sort.Slice(envVars, func(i, j int) bool {
		return envVars[i].Name < envVars[j].Name
	})
	return envVars
}

func ensureAcceptFileVars(content string) error {

	fileVars := findFileVars(content)

	for _, envVar := range fileVars {
		if envVar.Type != VarTypeEnv {
			continue
		}

		envVar := envVar.Name
		if os.Getenv(envVar) == "" && os.Getenv(envVar+"_POOL") == "" {
			return fmt.Errorf("missing required environment variable %q", envVar)
		}
	}
	return nil
}
