package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/config"
	"github.com/jecklgamis/resonate/internal/engine"
	"github.com/jecklgamis/resonate/internal/report"
)

func settingValue(rows []report.Setting, name string) string {
	for _, r := range rows {
		if r.Name == name {
			return r.Value
		}
	}
	return ""
}

func TestSafeURLDropsSecrets(t *testing.T) {
	cases := map[string]string{
		"http://user:pw@host:8080/a/b?token=zzz#frag": "http://host:8080/a/b…",
		"https://host/plain":                          "https://host/plain",
		"http://host/items/{{.Vars.id}}/x":            "http://host/items/…",
		"http://host/p?x={{.Feeder.k}}":               "http://host/p…",
		"localhost:50051":                             "localhost:50051",
	}
	for in, want := range cases {
		if got := safeURL(in); got != want {
			t.Errorf("safeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScenarioSettingsHTTPFlowListsShapeWithoutSecrets(t *testing.T) {
	s := &config.Scenario{
		Protocol: "http",
		HTTP: config.HTTPConfig{
			BaseURL:      "http://api.test",
			Timeout:      "5s",
			MaxIdleConns: 300,
			HTTP3:        true,
			Identities:   []map[string]string{{"token": "SECRET-A"}, {"token": "SECRET-B"}},
			Feeder:       config.FeederConfig{File: "users.csv"},
			Setup:        []config.HTTPTargetConfig{{URL: "/login?key=SECRET-C", Headers: map[string]string{"Authorization": "Bearer SECRET-D"}}},
			Flow: []config.HTTPTargetConfig{
				{URL: "/a"},
				{Repeat: 3, Steps: []config.HTTPTargetConfig{{URL: "/b"}, {URL: "/c"}}},
			},
		},
	}
	rows := scenarioSettings("does-not-exist.yaml", s)

	want := map[string]string{
		"Protocol":             "http",
		"Shape":                "3 flow steps, 1 setup step",
		"Identities":           "2",
		"Feeder":               "file users.csv (sequential)",
		"Transport":            "HTTP/3 (QUIC)",
		"Timeout":              "5s",
		"Max idle connections": "300",
	}
	for k, v := range want {
		if got := settingValue(rows, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got := settingValue(rows, "Target"); !strings.Contains(got, "http://api.test/a") || !strings.Contains(got, "/login…") {
		t.Errorf("Target = %q, want resolved hosts with the query stripped", got)
	}
	var all strings.Builder
	for _, r := range rows {
		all.WriteString(r.Name + "=" + r.Value + "\n")
	}
	if strings.Contains(all.String(), "SECRET") {
		t.Errorf("settings leak a secret:\n%s", all.String())
	}
}

func TestScenarioSettingsOtherProtocols(t *testing.T) {
	g := scenarioSettings("x.yaml", &config.Scenario{Protocol: "grpc", GRPC: config.GRPCConfig{
		Address: "localhost:50051", Plaintext: true, Calls: []config.GRPCCallConfig{{Method: "a/B"}, {Method: "a/C"}},
	}})
	if settingValue(g, "Connections") != "1" || settingValue(g, "Shape") != "2 calls per iteration" || settingValue(g, "Transport") != "plaintext" {
		t.Errorf("grpc settings wrong: %+v", g)
	}
	e := scenarioSettings("x.yaml", &config.Scenario{Protocol: "sse", SSE: config.SSEConfig{URL: "http://h/events?k=SECRET", Events: 5}})
	if settingValue(e, "Shape") != "one stream per iteration, 5 events" || strings.Contains(settingValue(e, "Target"), "SECRET") {
		t.Errorf("sse settings wrong: %+v", e)
	}
	k := scenarioSettings("x.yaml", &config.Scenario{HTTP: config.HTTPConfig{
		Targets: []config.HTTPTargetConfig{{URL: "http://h/"}},
		Feeder:  config.FeederConfig{Kafka: &config.KafkaFeederConfig{Topic: "users", Username: "u", Password: "SECRET"}},
	}})
	if got := settingValue(k, "Feeder"); got != "Kafka topic users" {
		t.Errorf("kafka feeder = %q", got)
	}
}

func TestHitSettings(t *testing.T) {
	rows := hitSettings("https://user:pw@h/x?t=1", 5*time.Second, 0, 0, false, true, false, "", "", false, false)
	if settingValue(rows, "Target") != "https://h/x…" || settingValue(rows, "Transport") != "HTTP/2 cleartext (h2c)" || settingValue(rows, "Timeout") != "5s" {
		t.Errorf("hit settings wrong: %+v", rows)
	}
}

func TestReportSettingsAddsBuildAndTimes(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := reportSettings([]report.Setting{{Name: "Target", Value: "x"}}, engineOptsForTest(), start, start.Add(time.Second))
	if settingValue(rows, "Target") != "x" || settingValue(rows, "Started") != "2026-01-02 03:04:05 UTC" || settingValue(rows, "Finished") != "2026-01-02 03:04:06 UTC" {
		t.Errorf("reportSettings wrong: %+v", rows)
	}
	if !strings.Contains(settingValue(rows, "Resonate"), "/") {
		t.Errorf("Resonate row should name the platform: %q", settingValue(rows, "Resonate"))
	}
	if rows[0].Name != "Target" {
		t.Errorf("what-was-tested rows should come first, got %q", rows[0].Name)
	}
}

func engineOptsForTest() engine.Options { return engine.Options{Duration: time.Second, Workers: 1} }
