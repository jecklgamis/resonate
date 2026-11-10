package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func loadScenarios(t *testing.T, yamlText string) *Scenario {
	t.Helper()
	s, err := Load(writeTempFile(t, "s.yaml", yamlText))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func resolveErr(t *testing.T, yamlText string) error {
	t.Helper()
	s, err := Load(writeTempFile(t, "s.yaml", yamlText))
	if err != nil {
		return err
	}
	_, err = s.Resolve()
	return err
}

const twoScenarios = `
protocol: http
http:
  base_url: http://shared
  timeout: 5s
  insecure: true
  max_idle_conns: 300
scenarios:
  zebra:
    load: {duration: 3s, workers: 2}
    http:
      targets: [{method: GET, url: /z}]
  apple:
    start_after: 1500ms
    load:
      stages:
        - {duration: 1s, workers: 4}
    http:
      timeout: 9s
      flow:
        - {name: one, method: GET, url: /a}
`

func TestScenariosKeepFileOrder(t *testing.T) {
	s := loadScenarios(t, twoScenarios)
	if got := strings.Join(s.Scenarios.Names(), ","); got != "zebra,apple" {
		t.Errorf("scenario order = %s, want zebra,apple (file order, not sorted)", got)
	}
	r, err := s.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if r[0].Name != "zebra" || r[1].Name != "apple" {
		t.Errorf("resolved order = %s,%s", r[0].Name, r[1].Name)
	}
}

func TestResolveMergesSharedSectionAndScenarioOverrides(t *testing.T) {
	s := loadScenarios(t, twoScenarios)
	r, err := s.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	zebra, apple := r[0].Config, r[1].Config

	// Inherited from the shared section.
	if zebra.HTTP.BaseURL != "http://shared" || !zebra.HTTP.Insecure || zebra.HTTP.MaxIdleConns != 300 {
		t.Errorf("zebra did not inherit the shared http section: %+v", zebra.HTTP)
	}
	if zebra.HTTP.Timeout != "5s" {
		t.Errorf("zebra timeout = %q, want the shared 5s", zebra.HTTP.Timeout)
	}
	// Overridden by the scenario, and only that key.
	if apple.HTTP.Timeout != "9s" {
		t.Errorf("apple timeout = %q, want its own 9s", apple.HTTP.Timeout)
	}
	if apple.HTTP.BaseURL != "http://shared" || !apple.HTTP.Insecure {
		t.Errorf("apple lost shared keys it did not override: %+v", apple.HTTP)
	}
	// Per-scenario content stays per-scenario.
	if len(zebra.HTTP.Targets) != 1 || len(zebra.HTTP.Flow) != 0 {
		t.Errorf("zebra targets/flow = %d/%d", len(zebra.HTTP.Targets), len(zebra.HTTP.Flow))
	}
	if len(apple.HTTP.Flow) != 1 || len(apple.HTTP.Targets) != 0 {
		t.Errorf("apple targets/flow = %d/%d", len(apple.HTTP.Targets), len(apple.HTTP.Flow))
	}
	// Each scenario has its own load profile.
	if zebra.Load.Duration != "3s" || zebra.Load.Workers != 2 || len(zebra.Load.Stages) != 0 {
		t.Errorf("zebra load = %+v", zebra.Load)
	}
	if len(apple.Load.Stages) != 1 || apple.Load.Stages[0].Workers != 4 || apple.Load.Duration != "" {
		t.Errorf("apple load = %+v", apple.Load)
	}
	if r[0].StartAfter != 0 || r[1].StartAfter != 1500*time.Millisecond {
		t.Errorf("start_after = %s, %s", r[0].StartAfter, r[1].StartAfter)
	}
}

// A scenario setting a key to its zero value must beat a non-zero shared one —
// the reason the merge is done on YAML nodes and not on structs.
func TestResolveScenarioCanOverrideWithZeroValue(t *testing.T) {
	s := loadScenarios(t, `
http:
  insecure: true
  timeout: 5s
  max_idle_conns: 300
scenarios:
  a:
    load: {duration: 1s, workers: 1}
    http:
      insecure: false
      timeout: ""
      max_idle_conns: 0
      targets: [{method: GET, url: "http://h/"}]
`)
	r, err := s.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	h := r[0].Config.HTTP
	if h.Insecure || h.Timeout != "" || h.MaxIdleConns != 0 {
		t.Errorf("zero-value overrides were lost: %+v", h)
	}
}

// Lists are replaced, never concatenated with the shared ones.
func TestResolveReplacesListsRatherThanMerging(t *testing.T) {
	s := loadScenarios(t, `
http:
  identities: [{u: shared1}, {u: shared2}]
scenarios:
  a:
    load: {duration: 1s, workers: 1}
    http:
      identities: [{u: mine}]
      flow: [{method: GET, url: "http://h/"}]
`)
	r, err := s.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if ids := r[0].Config.HTTP.Identities; len(ids) != 1 || ids[0]["u"] != "mine" {
		t.Errorf("identities = %v, want only the scenario's own", ids)
	}
}

func TestResolveProtocolPerScenario(t *testing.T) {
	s := loadScenarios(t, `
protocol: http
http:
  timeout: 7s
grpc:
  timeout: 3s
  plaintext: true
scenarios:
  web:
    load: {duration: 1s, workers: 1}
    http: {targets: [{method: GET, url: "http://h/"}]}
  rpc:
    protocol: grpc
    load: {duration: 1s, workers: 1}
    grpc:
      address: "h:1"
      calls: [{method: "a.B/C"}]
`)
	r, err := s.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if r[0].Config.Protocol != "http" || r[1].Config.Protocol != "grpc" {
		t.Errorf("protocols = %q, %q", r[0].Config.Protocol, r[1].Config.Protocol)
	}
	if r[0].Config.HTTP.Timeout != "7s" {
		t.Errorf("http scenario timeout = %q", r[0].Config.HTTP.Timeout)
	}
	// The grpc scenario takes the shared grpc section, not the http one.
	g := r[1].Config.GRPC
	if g.Timeout != "3s" || !g.Plaintext || g.Address != "h:1" {
		t.Errorf("grpc scenario = %+v", g)
	}
	if r[1].Config.HTTP.Timeout != "" {
		t.Errorf("grpc scenario leaked the http section: %+v", r[1].Config.HTTP)
	}
}

func TestResolveDefaultsProtocolToHTTP(t *testing.T) {
	s := loadScenarios(t, `
scenarios:
  a:
    load: {duration: 1s, workers: 1}
    http: {targets: [{method: GET, url: "http://h/"}]}
`)
	r, err := s.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if r[0].Config.Protocol != "http" {
		t.Errorf("protocol = %q, want http", r[0].Config.Protocol)
	}
}

func TestResolveRejectsBadFiles(t *testing.T) {
	ok := "    load: {duration: 1s, workers: 1}\n    http: {targets: [{method: GET, url: \"http://h/\"}]}\n"
	tests := []struct {
		name, yaml, want string
	}{
		{"unknown scenario key", "scenarios:\n  a:\n    start_afer: 1s\n" + ok, `unknown key "start_afer"`},
		{"unknown key in load", "scenarios:\n  a:\n    load: {duratoin: 1s}\n    http: {targets: []}\n", "duratoin"},
		{"unknown key in shared section", "http:\n  base_urll: x\nscenarios:\n  a:\n" + ok, "base_urll"},
		{"unknown key in scenario section", "scenarios:\n  a:\n    load: {duration: 1s}\n    http: {targetz: []}\n", "targetz"},
		{"duplicate names", "scenarios:\n  a:\n" + ok + "  a:\n" + ok, `scenario "a" is defined twice`},
		{"scenario not a mapping", "scenarios:\n  a: 3\n", "must be a mapping"},
		{"scenarios not a mapping", "scenarios: [a, b]\n", "scenarios must be a mapping"},
		{"top-level load", "load: {duration: 1s}\nscenarios:\n  a:\n" + ok, "top-level load"},
		{"bad start_after", "scenarios:\n  a:\n    start_after: soon\n" + ok, "invalid start_after"},
		{"negative start_after", "scenarios:\n  a:\n    start_after: -1s\n" + ok, "must not be negative"},
		{"unsupported protocol", "scenarios:\n  a:\n    protocol: smtp\n" + ok, `unsupported protocol "smtp"`},
		{"section for another protocol", "scenarios:\n  a:\n    protocol: http\n    load: {duration: 1s}\n    ws: {url: \"ws://h\"}\n", "has a ws section but its protocol is http"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := resolveErr(t, tt.yaml)
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// A single-scenario file is untouched by all of this: no scenarios, no merge.
func TestSingleScenarioFileHasNoScenarios(t *testing.T) {
	s := loadScenarios(t, "load: {duration: 1s}\nhttp: {targets: [{method: GET, url: \"http://h/\"}]}\n")
	if len(s.Scenarios) != 0 {
		t.Fatalf("Scenarios = %v", s.Scenarios)
	}
	if _, err := s.Resolve(); err == nil {
		t.Error("Resolve on a file with no scenarios should fail")
	}
}

func TestAssertionScenarioFieldLoads(t *testing.T) {
	s := loadScenarios(t, `
scenarios:
  a:
    load: {duration: 1s}
    http: {targets: [{method: GET, url: "http://h/"}]}
assertions:
  - {scenario: a, step: s, metric: success_rate, min: "0.9"}
`)
	if a := s.Assertions[0]; a.Scenario != "a" || a.Step != "s" {
		t.Errorf("assertion = %+v", a)
	}
}

// The JSON Schema describes a "partial" copy of each protocol section for the
// shared/override sections of a multi-scenario file. It must carry exactly the
// strict section's properties, or a key added to one would be rejected in the
// other.
func TestSchemaPartialSectionsMirrorStrictOnes(t *testing.T) {
	var schema struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal([]byte(ScenarioSchema()), &schema); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"http", "websocket", "grpc", "sse"} {
		strict, partial := schema.Defs[name], schema.Defs[name+"Partial"]
		if len(strict.Properties) == 0 {
			t.Fatalf("$defs.%s has no properties", name)
		}
		if len(partial.Required) != 0 {
			t.Errorf("$defs.%sPartial must require nothing, requires %v", name, partial.Required)
		}
		for key, want := range strict.Properties {
			got, ok := partial.Properties[key]
			if !ok {
				t.Errorf("$defs.%sPartial is missing property %q", name, key)
				continue
			}
			var a, b any
			_ = json.Unmarshal(want, &a)
			_ = json.Unmarshal(got, &b)
			if !reflect.DeepEqual(a, b) {
				t.Errorf("$defs.%sPartial property %q differs from $defs.%s", name, key, name)
			}
		}
		if len(partial.Properties) != len(strict.Properties) {
			t.Errorf("$defs.%sPartial has %d properties, $defs.%s has %d", name, len(partial.Properties), name, len(strict.Properties))
		}
	}
}

// Decoding a Scenario, changing it and marshalling it back is something tools
// and the docs tests do. It must keep working for every file, and the empty
// scenarios key must not appear (it would not load back).
func TestScenarioSurvivesAMarshalRoundTrip(t *testing.T) {
	t.Run("single-scenario file", func(t *testing.T) {
		s := loadScenarios(t, "load: {duration: 1s}\nhttp: {targets: [{method: GET, url: \"http://h/\"}]}\n")
		b, err := yaml.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "scenarios") {
			t.Errorf("a single-scenario file marshalled a scenarios key:\n%s", b)
		}
		back, err := Load(writeTempFile(t, "again.yaml", string(b)))
		if err != nil {
			t.Fatalf("the marshalled file doesn't load: %v\n%s", err, b)
		}
		if back.Load.Duration != "1s" || len(back.HTTP.Targets) != 1 {
			t.Errorf("round trip lost data: %+v", back)
		}
	})

	t.Run("multi-scenario file", func(t *testing.T) {
		s := loadScenarios(t, twoScenarios)
		want, err := s.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		b, err := yaml.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Load(writeTempFile(t, "again.yaml", string(b)))
		if err != nil {
			t.Fatalf("the marshalled file doesn't load: %v\n%s", err, b)
		}
		got, err := back.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		// Compared as YAML: a nil slice and an empty one are the same scenario.
		gotY, _ := yaml.Marshal(got)
		wantY, _ := yaml.Marshal(want)
		if string(gotY) != string(wantY) {
			t.Errorf("resolved scenarios changed across a round trip:\n got:\n%s\nwant:\n%s", gotY, wantY)
		}
		if strings.Join(back.Scenarios.Names(), ",") != "zebra,apple" {
			t.Errorf("order lost: %v", back.Scenarios.Names())
		}
	})
}
