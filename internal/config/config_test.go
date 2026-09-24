package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return path
}

func TestLoadFullScenario(t *testing.T) {
	path := writeTempFile(t, "scenario.yaml", `
protocol: http

load:
  duration: 30s
  rate: 50
  workers: 20
  iterations: 5

http:
  timeout: 5s
  insecure: true
  no_follow_redirects: true
  max_idle_conns: 200
  targets:
    - method: GET
      url: http://localhost:8080/health
      headers:
        User-Agent: resonate
`)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if s.Protocol != "http" {
		t.Errorf("Protocol = %q, want %q", s.Protocol, "http")
	}
	if s.Load.Rate != 50 {
		t.Errorf("Load.Rate = %v, want 50", s.Load.Rate)
	}
	if s.Load.Workers != 20 {
		t.Errorf("Load.Workers = %d, want 20", s.Load.Workers)
	}
	if s.Load.Iterations != 5 {
		t.Errorf("Load.Iterations = %d, want 5", s.Load.Iterations)
	}
	if !s.HTTP.Insecure || !s.HTTP.NoFollowRedirects {
		t.Errorf("Insecure/NoFollowRedirects not parsed as true")
	}
	if s.HTTP.MaxIdleConns != 200 {
		t.Errorf("MaxIdleConns = %d, want 200", s.HTTP.MaxIdleConns)
	}
	if len(s.HTTP.Targets) != 1 || s.HTTP.Targets[0].URL != "http://localhost:8080/health" {
		t.Errorf("Targets = %+v, unexpected", s.HTTP.Targets)
	}
	if s.HTTP.Targets[0].Headers["User-Agent"] != "resonate" {
		t.Errorf("target header not parsed correctly: %+v", s.HTTP.Targets[0].Headers)
	}
}

func TestLoadNonexistentFileErrors(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected an error loading a nonexistent file")
	}
}

func TestLoadInvalidYAMLErrors(t *testing.T) {
	path := writeTempFile(t, "bad.yaml", "load: [this is not: valid: yaml")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error parsing malformed YAML")
	}
}

func TestLoadConfigParsedDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"30s", 30 * time.Second, false},
		{"2m", 2 * time.Minute, false},
		{"not-a-duration", 0, true},
	}
	for _, c := range cases {
		l := LoadConfig{Duration: c.in}
		got, err := l.ParsedDuration()
		if c.wantErr {
			if err == nil {
				t.Errorf("ParsedDuration(%q): expected an error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsedDuration(%q) error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParsedDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestStageConfigParsedDuration(t *testing.T) {
	s := StageConfig{Duration: "15s"}
	got, err := s.ParsedDuration()
	if err != nil {
		t.Fatalf("ParsedDuration error: %v", err)
	}
	if got != 15*time.Second {
		t.Errorf("got %v, want 15s", got)
	}

	s2 := StageConfig{}
	got2, err := s2.ParsedDuration()
	if err != nil {
		t.Fatalf("ParsedDuration error: %v", err)
	}
	if got2 != 0 {
		t.Errorf("empty duration = %v, want 0", got2)
	}
}

func TestHTTPTargetConfigParsedPause(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"500ms", 500 * time.Millisecond, false},
		{"not-a-duration", 0, true},
	}
	for _, c := range cases {
		s := HTTPTargetConfig{Pause: c.in}
		got, err := s.ParsedPause()
		if c.wantErr {
			if err == nil {
				t.Errorf("ParsedPause(%q): expected an error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsedPause(%q) error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParsedPause(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestHTTPTargetConfigParsedPauseMax(t *testing.T) {
	s := HTTPTargetConfig{PauseMax: "1.5s"}
	got, err := s.ParsedPauseMax()
	if err != nil {
		t.Fatalf("ParsedPauseMax error: %v", err)
	}
	if got != 1500*time.Millisecond {
		t.Errorf("got %v, want 1.5s", got)
	}

	empty := HTTPTargetConfig{}
	got2, err := empty.ParsedPauseMax()
	if err != nil {
		t.Fatalf("ParsedPauseMax error: %v", err)
	}
	if got2 != 0 {
		t.Errorf("empty pause_max = %v, want 0", got2)
	}

	invalid := HTTPTargetConfig{PauseMax: "not-a-duration"}
	if _, err := invalid.ParsedPauseMax(); err == nil {
		t.Error("expected an error for an invalid pause_max")
	}
}

func TestHTTPTargetConfigParsedDuring(t *testing.T) {
	s := HTTPTargetConfig{During: "10s"}
	got, err := s.ParsedDuring()
	if err != nil {
		t.Fatalf("ParsedDuring error: %v", err)
	}
	if got != 10*time.Second {
		t.Errorf("got %v, want 10s", got)
	}

	empty := HTTPTargetConfig{}
	got2, err := empty.ParsedDuring()
	if err != nil {
		t.Fatalf("ParsedDuring error: %v", err)
	}
	if got2 != 0 {
		t.Errorf("empty during = %v, want 0", got2)
	}

	invalid := HTTPTargetConfig{During: "not-a-duration"}
	if _, err := invalid.ParsedDuring(); err == nil {
		t.Error("expected an error for an invalid during")
	}
}

func TestResolveBodyInline(t *testing.T) {
	target := HTTPTargetConfig{Body: `{"id":1}`}
	got, err := target.ResolveBody()
	if err != nil {
		t.Fatalf("ResolveBody error: %v", err)
	}
	if got != `{"id":1}` {
		t.Errorf("got %q, want the inline body unchanged", got)
	}
}

func TestResolveBodyFromFile(t *testing.T) {
	path := writeTempFile(t, "body.json", `{"id":{{.Seq}}}`)
	target := HTTPTargetConfig{BodyFile: path}
	got, err := target.ResolveBody()
	if err != nil {
		t.Fatalf("ResolveBody error: %v", err)
	}
	if got != `{"id":{{.Seq}}}` {
		t.Errorf("got %q, want the file contents (still a template, unrendered)", got)
	}
}

func TestResolveBodyFileTakesPrecedenceOverInlineBody(t *testing.T) {
	path := writeTempFile(t, "body.json", "from-file")
	target := HTTPTargetConfig{Body: "from-inline", BodyFile: path}
	got, err := target.ResolveBody()
	if err != nil {
		t.Fatalf("ResolveBody error: %v", err)
	}
	if got != "from-file" {
		t.Errorf("got %q, want BodyFile to take precedence over inline Body", got)
	}
}

func TestResolveBodyMissingFileErrors(t *testing.T) {
	target := HTTPTargetConfig{BodyFile: filepath.Join(t.TempDir(), "missing.json")}
	_, err := target.ResolveBody()
	if err == nil {
		t.Fatal("expected an error reading a nonexistent body file")
	}
}

func TestLoadTargetRawBodyFile(t *testing.T) {
	path := writeTempFile(t, "scenario.yaml", `
protocol: http

load:
  duration: 10s

http:
  targets:
    - method: POST
      url: http://localhost:8080/upload
      raw_body_file: /path/to/payload.bin
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if len(s.HTTP.Targets) != 1 {
		t.Fatalf("Targets len = %d, want 1", len(s.HTTP.Targets))
	}
	if s.HTTP.Targets[0].RawBodyFile != "/path/to/payload.bin" {
		t.Errorf("RawBodyFile = %q, want /path/to/payload.bin", s.HTTP.Targets[0].RawBodyFile)
	}
}

func TestLoadHTTPBaseURL(t *testing.T) {
	path := writeTempFile(t, "scenario.yaml", `
protocol: http

load:
  duration: 10s

http:
  base_url: http://localhost:8080
  targets:
    - url: /health
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if s.HTTP.BaseURL != "http://localhost:8080" {
		t.Errorf("BaseURL = %q, want http://localhost:8080", s.HTTP.BaseURL)
	}
}

func TestLoadFlowAndIdentities(t *testing.T) {
	path := writeTempFile(t, "flow.yaml", `
protocol: http

load:
  duration: 10s

http:
  identities:
    - username: alice
      account_id: "1001"
    - username: bob
      account_id: "1002"

  setup:
    - method: POST
      url: http://localhost:8080/login
      body: '{"username":"{{.Identity.username}}"}'
      extract:
        token: json:token

  flow:
    - method: POST
      url: http://localhost:8080/orders
      headers:
        Authorization: "Bearer {{.Vars.token}}"
      extract:
        order_id: json:id
    - method: GET
      url: "http://localhost:8080/orders/{{.Vars.order_id}}"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if len(s.HTTP.Identities) != 2 {
		t.Fatalf("Identities len = %d, want 2", len(s.HTTP.Identities))
	}
	if s.HTTP.Identities[0]["username"] != "alice" {
		t.Errorf("Identities[0] = %+v, want username=alice", s.HTTP.Identities[0])
	}
	if len(s.HTTP.Setup) != 1 {
		t.Fatalf("Setup len = %d, want 1", len(s.HTTP.Setup))
	}
	if s.HTTP.Setup[0].Extract["token"] != "json:token" {
		t.Errorf("Setup[0].Extract = %+v, want token=json:token", s.HTTP.Setup[0].Extract)
	}
	if len(s.HTTP.Flow) != 2 {
		t.Fatalf("Flow len = %d, want 2", len(s.HTTP.Flow))
	}
	if s.HTTP.Flow[0].Extract["order_id"] != "json:id" {
		t.Errorf("Flow[0].Extract = %+v, want order_id=json:id", s.HTTP.Flow[0].Extract)
	}
}

func TestLoadAssertions(t *testing.T) {
	path := writeTempFile(t, "assertions.yaml", `
protocol: http

load:
  requests: 10

http:
  targets:
    - url: http://localhost:8080/health

assertions:
  - metric: success_rate
    min: "0.99"
  - metric: latency_p95
    max: 500ms
  - metric: rate
    min: "10"
    max: "1000"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if len(s.Assertions) != 3 {
		t.Fatalf("Assertions len = %d, want 3", len(s.Assertions))
	}
	if s.Assertions[0].Metric != "success_rate" || s.Assertions[0].Min != "0.99" {
		t.Errorf("Assertions[0] = %+v, unexpected", s.Assertions[0])
	}
	if s.Assertions[1].Metric != "latency_p95" || s.Assertions[1].Max != "500ms" {
		t.Errorf("Assertions[1] = %+v, unexpected", s.Assertions[1])
	}
	if s.Assertions[2].Min != "10" || s.Assertions[2].Max != "1000" {
		t.Errorf("Assertions[2] = %+v, want both bounds set", s.Assertions[2])
	}
}

func TestLoadWSScenario(t *testing.T) {
	path := writeTempFile(t, "ws.yaml", `
protocol: ws

load:
  duration: 10s

ws:
  url: ws://localhost:8080/socket
  timeout: 5s
  headers:
    Authorization: "Bearer {{.Identity.token}}"
  identities:
    - token: abc123
  messages:
    - body: '{"type":"ping"}'
      wait: true
      extract:
        pong: json:pong
    - body: '{"type":"ack","ref":"{{.Vars.pong}}"}'
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if s.Protocol != "ws" {
		t.Errorf("Protocol = %q, want ws", s.Protocol)
	}
	if s.WS.URL != "ws://localhost:8080/socket" {
		t.Errorf("WS.URL = %q, unexpected", s.WS.URL)
	}
	if len(s.WS.Messages) != 2 {
		t.Fatalf("WS.Messages len = %d, want 2", len(s.WS.Messages))
	}
	if !s.WS.Messages[0].Wait {
		t.Error("Messages[0].Wait = false, want true")
	}
	if s.WS.Messages[0].Extract["pong"] != "json:pong" {
		t.Errorf("Messages[0].Extract = %+v, want pong=json:pong", s.WS.Messages[0].Extract)
	}
	if len(s.WS.Identities) != 1 || s.WS.Identities[0]["token"] != "abc123" {
		t.Errorf("WS.Identities = %+v, unexpected", s.WS.Identities)
	}
	d, err := s.WS.ParsedTimeout()
	if err != nil {
		t.Fatalf("ParsedTimeout error: %v", err)
	}
	if d != 5*time.Second {
		t.Errorf("ParsedTimeout = %v, want 5s", d)
	}
}

func TestWSConfigParsedTimeoutEmpty(t *testing.T) {
	d, err := (WSConfig{}).ParsedTimeout()
	if err != nil {
		t.Fatalf("ParsedTimeout error: %v", err)
	}
	if d != 0 {
		t.Errorf("got %v, want 0 for an unset timeout", d)
	}
}

func TestLoadStages(t *testing.T) {
	path := writeTempFile(t, "stages.yaml", `
load:
  stages:
    - duration: 30s
      workers: 50
      rate: 50
    - duration: 15s
      workers: 0
      rate: 0

http:
  targets:
    - url: http://localhost:8080/health
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if len(s.Load.Stages) != 2 {
		t.Fatalf("Stages len = %d, want 2", len(s.Load.Stages))
	}
	d0, err := s.Load.Stages[0].ParsedDuration()
	if err != nil {
		t.Fatalf("ParsedDuration error: %v", err)
	}
	if d0 != 30*time.Second {
		t.Errorf("Stages[0] duration = %v, want 30s", d0)
	}
	if s.Load.Stages[1].Workers != 0 || s.Load.Stages[1].Rate != 0 {
		t.Errorf("Stages[1] = %+v, want zero workers/rate (ramp-down)", s.Load.Stages[1])
	}
}
