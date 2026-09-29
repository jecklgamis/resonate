package config

import _ "embed"

//go:embed scenario.schema.json
var scenarioSchema string

// ScenarioSchema returns the JSON Schema for scenario YAML files.
func ScenarioSchema() string {
	return scenarioSchema
}
