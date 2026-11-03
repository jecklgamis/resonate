package cli

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/jecklgamis/resonate/internal/config"
	"github.com/jecklgamis/resonate/internal/engine"
)

// grpcHealthServer serves grpc-go's standard health service ("up" is
// SERVING, anything unregistered NOT_FOUND) plus reflection, on a random port.
func grpcHealthServer(t *testing.T) string {
	t.Helper()
	srv := grpc.NewServer()
	hs := health.NewServer()
	hs.SetServingStatus("up", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(srv, hs)
	reflection.Register(srv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

type grpcSummary struct {
	Requests    int     `json:"requests"`
	SuccessRate float64 `json:"success_rate"`
}

// grpcRunJSON runs `resonate run <args> --json` in-process and decodes the summary.
func grpcRunJSON(t *testing.T, args ...string) grpcSummary {
	t.Helper()
	cmd := newRunCommand()
	cmd.SetArgs(append(args, "--json"))
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	var s grpcSummary
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("output isn't valid JSON: %v\noutput: %s", err, out)
	}
	return s
}

func TestRunCommandGRPCScenarioEndToEnd(t *testing.T) {
	t.Chdir(t.TempDir())
	addr := grpcHealthServer(t)

	path := writeTempScenario(t, `
protocol: grpc

load:
  requests: 3
  workers: 1

grpc:
  address: `+addr+`
  plaintext: true
  timeout: 3s
  metadata:
    x-run: "{{.Seq}}"
  calls:
    - method: grpc.health.v1.Health/Check
      message: '{"service":"up"}'
      extract:
        st: json:status
      expect_body:
        json:status: SERVING
    - method: grpc.health.v1.Health/Check
      message: '{"service":"{{.Vars.st}}"}'   # "SERVING" isn't a service: NOT_FOUND
      expect_code: [NOT_FOUND]
`)
	s := grpcRunJSON(t, path)
	// 3 iterations * 2 calls.
	if s.Requests != 6 {
		t.Errorf("Requests = %d, want 6 (3 iterations * 2 calls)", s.Requests)
	}
	if s.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", s.SuccessRate)
	}
}

func TestRunCommandGRPCUnexpectedStatusIsAFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	addr := grpcHealthServer(t)

	path := writeTempScenario(t, `
protocol: grpc
load:
  requests: 2
  workers: 1
grpc:
  address: `+addr+`
  plaintext: true
  calls:
    - method: grpc.health.v1.Health/Check
      message: '{"service":"nope"}'
`)
	if s := grpcRunJSON(t, path); s.Requests != 2 || s.SuccessRate != 0 {
		t.Errorf("summary = %+v, want 2 requests all failing (NOT_FOUND)", s)
	}
}

func TestRunCommandGRPCMessageFile(t *testing.T) {
	t.Chdir(t.TempDir())
	addr := grpcHealthServer(t)
	msg := filepath.Join(t.TempDir(), "msg.json")
	if err := os.WriteFile(msg, []byte(`{"service":"up"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	path := writeTempScenario(t, `
protocol: grpc
load:
  requests: 1
  workers: 1
grpc:
  address: `+addr+`
  plaintext: true
  calls:
    - method: grpc.health.v1.Health/Check
      message_file: `+msg+`
      expect_body:
        json:status: SERVING
`)
	if s := grpcRunJSON(t, path); s.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", s.SuccessRate)
	}
}

func TestRunCommandGRPCConfigErrors(t *testing.T) {
	addr := grpcHealthServer(t)
	call := "    - method: grpc.health.v1.Health/Check\n      message: '{\"service\":\"up\"}'\n"

	cases := map[string]struct{ yaml, want string }{
		"missing address":      {"grpc:\n  plaintext: true\n  calls:\n" + call, "grpc.address is required"},
		"missing calls":        {"grpc:\n  address: " + addr + "\n  plaintext: true\n", "grpc.calls must have at least one entry"},
		"bad timeout":          {"grpc:\n  address: " + addr + "\n  plaintext: true\n  timeout: soon\n  calls:\n" + call, "invalid grpc.timeout"},
		"unknown method":       {"grpc:\n  address: " + addr + "\n  plaintext: true\n  calls:\n    - method: grpc.health.v1.Health/Nope\n", "has no method"},
		"stream kind mismatch": {"grpc:\n  address: " + addr + "\n  plaintext: true\n  calls:\n    - method: grpc.health.v1.Health/Watch\n      messages: ['{}']\n", "server-streaming"},
		"unary with receive":   {"grpc:\n  address: " + addr + "\n  plaintext: true\n  calls:\n    - method: grpc.health.v1.Health/Check\n      receive: 2\n", "unary RPC"},
		"bad call duration":    {"grpc:\n  address: " + addr + "\n  plaintext: true\n  calls:\n    - method: grpc.health.v1.Health/Watch\n      duration: soon\n", "grpc.calls[0].duration"},
		"bad send_interval":    {"grpc:\n  address: " + addr + "\n  plaintext: true\n  calls:\n    - method: grpc.health.v1.Health/Watch\n      send_interval: soon\n", "grpc.calls[0].send_interval"},
		"bad expect_code":      {"grpc:\n  address: " + addr + "\n  plaintext: true\n  calls:\n    - method: grpc.health.v1.Health/Check\n      expect_code: [MAYBE]\n", "invalid expect_code"},
		"plaintext + ca":       {"grpc:\n  address: " + addr + "\n  plaintext: true\n  ca_file: ca.pem\n  calls:\n" + call, "plaintext"},
		"unreadable msg":       {"grpc:\n  address: " + addr + "\n  plaintext: true\n  calls:\n    - method: grpc.health.v1.Health/Check\n      message_file: /nonexistent/msg.json\n", "message_file"},
		"proto and descset":    {"grpc:\n  address: " + addr + "\n  plaintext: true\n  proto_files: [a.proto]\n  descriptor_set: a.pb\n  calls:\n" + call, "mutually exclusive"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			path := writeTempScenario(t, "protocol: grpc\nload:\n  requests: 1\n  workers: 1\n"+tc.yaml)
			cmd := newRunCommand()
			cmd.SetArgs([]string{path, "--quiet"})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRunCommandUnknownProtocolListsGRPC(t *testing.T) {
	t.Chdir(t.TempDir())
	path := writeTempScenario(t, "protocol: carrier-pigeon\nload:\n  requests: 1\n")
	cmd := newRunCommand()
	cmd.SetArgs([]string{path})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), `"grpc"`) {
		t.Fatalf("want the unsupported-protocol error to list grpc, got %v", err)
	}
}

func TestRunCommandGRPCServerStreamingScenario(t *testing.T) {
	t.Chdir(t.TempDir())
	addr := grpcHealthServer(t)

	// Health/Watch is a real server-streaming RPC: it sends the current
	// status immediately, then again on every change. receive: 1 ends it.
	path := writeTempScenario(t, `
protocol: grpc

load:
  requests: 3
  workers: 1

grpc:
  address: `+addr+`
  plaintext: true
  timeout: 3s
  calls:
    - method: grpc.health.v1.Health/Watch
      message: '{"service":"up"}'
      receive: 1
      expect_body:
        json:status: SERVING
`)
	s := grpcRunJSON(t, path)
	// 3 iterations * 1 received message.
	if s.Requests != 3 || s.SuccessRate != 1 {
		t.Errorf("summary = %+v, want 3 successful requests (one message per stream)", s)
	}
}

func TestRunCommandGRPCStreamWithoutBoundFailsAfterTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	addr := grpcHealthServer(t)

	// Watch never ends on its own and no receive/duration is set, so the
	// stream is still open at the timeout: a failure that says what to set.
	path := writeTempScenario(t, `
protocol: grpc
load:
  requests: 1
  workers: 1
grpc:
  address: `+addr+`
  plaintext: true
  timeout: 200ms
  calls:
    - method: grpc.health.v1.Health/Watch
      message: '{"service":"up"}'
`)
	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--json", "--quiet"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	if !strings.Contains(out, "set receive or duration") {
		t.Errorf("report should carry the unbounded-stream error, got:\n%s", out)
	}
}

// The shipped no-reflection example must keep working: its .proto has to
// compile (including the google/protobuf/timestamp.proto import) and every
// method the example scenario calls has to resolve. No server is needed —
// with proto_files the schema never touches the network.
func TestShippedProtoFilesExampleBuilds(t *testing.T) {
	scenario, err := config.Load("../../examples/grpc-proto-files.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// proto_files paths are relative to where resonate runs: the repo root.
	t.Chdir("../..")
	scenario.GRPC.Address = "127.0.0.1:1" // never dialed during construction

	g, err := buildGRPCGenerator(scenario.GRPC)
	if err != nil {
		t.Fatalf("examples/grpc-proto-files.yaml no longer builds: %v", err)
	}
	g.Close()
}

// examples/grpc-kvstore.yaml targets a server we can't start here (it needs
// reflection from the go-grpc-example project), so check everything that
// doesn't need one: it parses, its load profile and assertions are valid, and
// it still names the calls the comments describe.
func TestShippedKVStoreExampleIsValid(t *testing.T) {
	s, err := config.Load("../../examples/grpc-kvstore.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if s.Protocol != "grpc" || s.GRPC.Address != "localhost:4000" || !s.GRPC.Plaintext {
		t.Fatalf("unexpected target: protocol=%q grpc=%+v", s.Protocol, s.GRPC)
	}
	opts, err := loadOptionsFrom(s.Load)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Validate(opts); err != nil {
		t.Fatalf("load profile invalid: %v", err)
	}
	if _, err := assertionsFrom(s.Assertions); err != nil {
		t.Fatalf("assertions invalid: %v", err)
	}
	var methods []string
	for _, c := range s.GRPC.Calls {
		methods = append(methods, c.Method)
	}
	if strings.Join(methods, ",") != "kvstore.KeyValueStore/Put,kvstore.KeyValueStore/Get" {
		t.Fatalf("calls = %v, want Put then Get", methods)
	}
}
