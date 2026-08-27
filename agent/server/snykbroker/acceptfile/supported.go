package acceptfile

import (
	"go.uber.org/zap"
)

// Nothing in an accept file stops the agent.
//
// Enabling the tunnel switches deployments that are running on snyk-broker
// today, and that switch has to be transparent: a file the broker accepts must
// still start. Anything the Router cannot carry is warned about and ignored, so
// an operator relying on it finds out from the log rather than from an agent
// that will not boot, and can stay on snyk-broker until we implement it.
//
// What ends up here are constructs snyk-broker honours that the Router does not
// implement — body and query "valid" filters, requiredCapabilities. Ignoring
// one widens the rule, so the warning says exactly that.
// The one thing still refused is a malformed wildcard origin, and that is not a
// migration risk: the snyk-broker path already refuses it too, at render
// (ErrWildcardOriginRequiresTLSVerification, the invalid-origin error) or by
// panicking when the reflector is disabled. No working deployment has one, and
// treating a bad family as permissive would authorize hosts nobody chose.

// warnIgnoredPublicRules logs once for an accept file that declares inbound
// rules, and reports how many it found.
//
// A "public" block describes webhook traffic the relay does not carry, so it
// cannot widen what the agent will call outbound — and enough files carry one
// copied from a snyk-broker config that refusing them would break working
// deployments over a section that routes nothing.
//
// An empty block is silent: it is what Render itself emits, and warning about
// it would train everyone to ignore the warning.
func warnIgnoredPublicRules(dict map[string]any, logger *zap.Logger) int {
	rules, ok := dict[RULES_PUBLIC].([]any)
	if !ok || len(rules) == 0 {
		return 0
	}
	logger.Warn(
		"Ignoring inbound rules in the accept file: the relay carries no inbound traffic, "+
			"so these route nothing. Support for the section will be removed — remove it from your accept file.",
		zap.String("section", RULES_PUBLIC),
		zap.Int("rules", len(rules)),
	)
	return len(rules)
}

// warnUnsupportedRule logs whatever in a rule the Router will not act on, and
// reports how many warnings it emitted.
func warnUnsupportedRule(rule map[string]any, logger *zap.Logger) int {
	path, _ := rule["path"].(string)
	log := logger.With(zap.String("rulePath", path))
	warnings := 0

	if _, present := rule["requiredCapabilities"]; present {
		warnings++
		log.Warn(
			"Ignoring \"requiredCapabilities\" on an accept file rule: the relay negotiates no " +
				"client capabilities, so the rule is allowed through unconditionally. " +
				"snyk-broker would reject a request that did not meet them.")
	}

	validEntries, ok := rule["valid"].([]any)
	if !ok {
		return warnings
	}
	for _, entry := range validEntries {
		entryDict, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if key := unsupportedValidKey(entryDict); key != "" {
			warnings++
			log.Warn(
				"Ignoring a \"valid\" entry on an accept file rule: the relay does not inspect the "+
					"request body or query string, so this narrowing is not applied and the rule "+
					"matches more than it does under snyk-broker.",
				zap.String("filter", key))
		}
	}
	return warnings
}

// unsupportedValidKey names why a "valid" entry cannot be honoured, or "" when
// it is a header requirement the matcher does apply.
func unsupportedValidKey(entry map[string]any) string {
	for _, key := range []string{"path", "regex", "queryParam"} {
		if _, present := entry[key]; present {
			return key
		}
	}
	if header, _ := entry["header"].(string); header == "" {
		return "no recognized key"
	}
	return ""
}
