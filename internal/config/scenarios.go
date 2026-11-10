package config

import (
	"bytes"
	"fmt"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// scenarioSections are the protocol sections a scenario may carry; each one
// inherits the file's top-level section of the same name.
var scenarioSections = []string{"http", "ws", "grpc", "sse"}

// ScenarioSet is the ordered `scenarios:` map of a multi-scenario file. It
// keeps the order the scenarios were written in (a Go map would not), which is
// the order the report lists them in.
type ScenarioSet []ScenarioEntry

// ScenarioEntry is one named entry under `scenarios:`.
//
// A scenario is the same thing a single-scenario file is — its own protocol,
// load profile and protocol section — plus StartAfter. Its protocol section
// (http/ws/grpc/sse) is kept as written and merged over the file's top-level
// section of the same name by Scenario.Resolve; the merge is by top-level
// key, so a key set in the scenario replaces the shared one outright (lists
// and maps are not combined).
type ScenarioEntry struct {
	Name string

	// Protocol overrides the file's protocol for this scenario ("http", "ws",
	// "grpc" or "sse"); empty means the file's.
	Protocol string

	// StartAfter delays this scenario's start relative to the run's start
	// (a Go duration; empty or "0s" means start immediately).
	StartAfter string

	// Load is this scenario's own load profile — the whole point of having
	// several. Files do not share a load: every scenario sets its own.
	Load LoadConfig

	sections map[string]*yaml.Node
}

// ParsedStartAfter parses StartAfter, returning 0 if unset.
func (e ScenarioEntry) ParsedStartAfter() (time.Duration, error) {
	if e.StartAfter == "" {
		return 0, nil
	}
	return time.ParseDuration(e.StartAfter)
}

// scenarioEntryFields is the set of keys a scenario accepts, checked by hand
// because the entry is decoded piecewise (see UnmarshalYAML).
var scenarioEntryFields = append([]string{"protocol", "start_after", "load"}, scenarioSections...)

// UnmarshalYAML decodes the `scenarios:` mapping, keeping its order and
// rejecting an unknown key inside a scenario (a typo such as `start_afer:`
// would otherwise silently start the scenario at once).
func (ss *ScenarioSet) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: scenarios must be a mapping of scenario name to its definition", node.Line)
	}
	var set ScenarioSet
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		name := key.Value
		if name == "" {
			return fmt.Errorf("line %d: a scenario needs a non-empty name", key.Line)
		}
		for _, prev := range set {
			if prev.Name == name {
				return fmt.Errorf("line %d: scenario %q is defined twice", key.Line, name)
			}
		}
		if val.Kind != yaml.MappingNode {
			return fmt.Errorf("line %d: scenario %q must be a mapping (protocol/start_after/load/http/...)", val.Line, name)
		}

		e := ScenarioEntry{Name: name, sections: map[string]*yaml.Node{}}
		for j := 0; j+1 < len(val.Content); j += 2 {
			k, v := val.Content[j], val.Content[j+1]
			var err error
			switch k.Value {
			case "protocol":
				err = v.Decode(&e.Protocol)
			case "start_after":
				err = v.Decode(&e.StartAfter)
			case "load":
				err = decodeStrict(v, &e.Load)
			case "http", "ws", "grpc", "sse":
				if v.Kind != yaml.MappingNode {
					return fmt.Errorf("line %d: scenario %q: %s must be a mapping", v.Line, name, k.Value)
				}
				e.sections[k.Value] = v
			default:
				return fmt.Errorf("line %d: scenario %q: unknown key %q (expected one of %v)", k.Line, name, k.Value, scenarioEntryFields)
			}
			if err != nil {
				return fmt.Errorf("scenario %q: %s: %w", name, k.Value, err)
			}
		}
		set = append(set, e)
	}
	*ss = set
	return nil
}

// MarshalYAML writes the set back as the mapping it was read from, so a
// Scenario survives a yaml.Marshal / Load round trip (tools and tests that
// decode a scenario, adjust it and write it out rely on that).
func (ss ScenarioSet) MarshalYAML() (any, error) {
	out := &yaml.Node{Kind: yaml.MappingNode}
	scalar := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }
	for _, e := range ss {
		body := &yaml.Node{Kind: yaml.MappingNode}
		add := func(key string, val *yaml.Node) { body.Content = append(body.Content, scalar(key), val) }
		if e.Protocol != "" {
			add("protocol", scalar(e.Protocol))
		}
		if e.StartAfter != "" {
			add("start_after", scalar(e.StartAfter))
		}
		var load yaml.Node
		if err := load.Encode(e.Load); err != nil {
			return nil, err
		}
		add("load", &load)
		for _, sec := range scenarioSections {
			if n, ok := e.sections[sec]; ok {
				add(sec, n)
			}
		}
		out.Content = append(out.Content, scalar(e.Name), body)
	}
	return out, nil
}

// ResolvedScenario is one scenario of a multi-scenario file, fully merged:
// Config is an ordinary single-scenario Scenario (its Assertions are not set;
// assertions belong to the file) that the single-scenario code can build as
// is.
type ResolvedScenario struct {
	Name       string
	StartAfter time.Duration
	Config     Scenario
}

// Names lists the scenarios in file order.
func (ss ScenarioSet) Names() []string {
	names := make([]string, len(ss))
	for i, e := range ss {
		names[i] = e.Name
	}
	return names
}

// Resolve merges every scenario with the file's shared sections and returns
// them in file order. It is an error for the file to also carry its own
// top-level load (each scenario brings its own), or for a scenario to have a
// section its protocol doesn't use.
func (s *Scenario) Resolve() ([]ResolvedScenario, error) {
	if len(s.Scenarios) == 0 {
		return nil, fmt.Errorf("no scenarios defined")
	}
	if !isZeroLoad(s.Load) {
		return nil, fmt.Errorf("a file with scenarios: sets load inside each scenario; the top-level load: is not used")
	}

	shared := map[string]any{"http": s.HTTP, "ws": s.WS, "grpc": s.GRPC, "sse": s.SSE}
	resolved := make([]ResolvedScenario, 0, len(s.Scenarios))
	for _, e := range s.Scenarios {
		proto := e.Protocol
		if proto == "" {
			proto = s.Protocol
		}
		if proto == "" {
			proto = "http"
		}
		if !slices.Contains(scenarioSections, proto) {
			return nil, fmt.Errorf("scenario %q: unsupported protocol %q (expected \"http\", \"ws\", \"grpc\" or \"sse\")", e.Name, proto)
		}
		for sec := range e.sections {
			if sec != proto {
				return nil, fmt.Errorf("scenario %q: has a %s section but its protocol is %s", e.Name, sec, proto)
			}
		}
		after, err := e.ParsedStartAfter()
		if err != nil {
			return nil, fmt.Errorf("scenario %q: invalid start_after %q: %w", e.Name, e.StartAfter, err)
		}
		if after < 0 {
			return nil, fmt.Errorf("scenario %q: start_after must not be negative", e.Name)
		}

		cfg := Scenario{Protocol: proto, Load: e.Load}
		var target any
		switch proto {
		case "http":
			target = &cfg.HTTP
		case "ws":
			target = &cfg.WS
		case "grpc":
			target = &cfg.GRPC
		case "sse":
			target = &cfg.SSE
		}
		if err := mergeSection(shared[proto], e.sections[proto], target); err != nil {
			return nil, fmt.Errorf("scenario %q: %s: %w", e.Name, proto, err)
		}
		resolved = append(resolved, ResolvedScenario{Name: e.Name, StartAfter: after, Config: cfg})
	}
	return resolved, nil
}

// isZeroLoad reports whether l is the zero LoadConfig (nothing set).
func isZeroLoad(l LoadConfig) bool {
	return l.Duration == "" && l.Requests == 0 && l.Rate == 0 && l.Workers == 0 &&
		l.MaxWorkers == 0 && len(l.Stages) == 0 && l.Iterations == 0
}

// mergeSection decodes into out the shared section with the scenario's own
// keys laid over it. Both go through YAML nodes so that a key the scenario
// sets to its zero value (http3: false, base_url: "") still overrides a
// shared non-zero one, which a struct-level merge could not tell apart from
// "not set".
func mergeSection(shared any, overlay *yaml.Node, out any) error {
	var base yaml.Node
	if err := base.Encode(shared); err != nil {
		return err
	}
	if overlay != nil {
		for i := 0; i+1 < len(overlay.Content); i += 2 {
			setKey(&base, overlay.Content[i], overlay.Content[i+1])
		}
	}
	return decodeStrict(&base, out)
}

// setKey sets key to val in the mapping node m, replacing an existing entry or
// appending a new one.
func setKey(m *yaml.Node, key, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key.Value {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, key, val)
}

// decodeStrict decodes node into out, rejecting keys out has no field for.
// (Node.Decode alone would ignore them: it does not inherit the caller's
// KnownFields setting.)
func decodeStrict(node *yaml.Node, out any) error {
	b, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(out)
}
