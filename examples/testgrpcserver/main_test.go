package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jecklgamis/resonate/internal/cli"
	"github.com/jecklgamis/resonate/internal/config"
	"github.com/jecklgamis/resonate/internal/generator"
)

// startServer runs the example server on a random local port.
func startServer(t *testing.T, opts options) string {
	t.Helper()
	srv, err := newServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

var defaults = options{reflection: true, eventInterval: 2 * time.Millisecond}

type summary struct {
	Requests    int     `json:"requests"`
	SuccessRate float64 `json:"success_rate"`
}

// repoRoot finds the module root by walking up from the working directory, so
// it is safe to call again after a previous call already changed into it.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && filepath.Base(dir) != "dsl-examples" {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the working directory")
		}
		dir = parent
	}
}

// runExample runs one of the shipped example scenarios, unchanged apart from
// pointing it at addr and shortening the load to a few iterations, through the
// real `resonate run` command.
func runExample(t *testing.T, file, addr string, iterations uint64) (summary, error) {
	t.Helper()
	return runScenario(t, filepath.Join("examples", file), addr, iterations)
}

// runScenario is runExample for any scenario file, given relative to the repo root.
func runScenario(t *testing.T, relPath, addr string, iterations uint64) (summary, error) {
	t.Helper()
	t.Chdir(repoRoot(t)) // proto_files paths in the examples are relative to the repo root

	sc, err := config.Load(relPath)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Base(relPath)
	sc.GRPC.Address = addr
	sc.Load = config.LoadConfig{Requests: iterations, Workers: 2}
	b, err := yaml.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), file)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := cli.NewRootCommand("test")
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"run", path, "--json", "--quiet", "--html-report", "", "--json-report", "", "--results-file", ""})

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := cmd.Execute()
	w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	if runErr != nil {
		return summary{}, runErr
	}

	var s summary
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("output isn't valid JSON: %v\n%s", err, out)
	}
	return s, nil
}

func TestShippedExamplesRunAgainstTheExampleServer(t *testing.T) {
	addr := startServer(t, defaults)

	const iterations = 4
	for _, tc := range []struct {
		file            string
		requestsPerIter int
		why             string
	}{
		{"grpc-basic.yaml", 2, "CreateUser + GetUser"},
		{"grpc-proto-files.yaml", 2, "CreateUser + GetUser"},
		// Subscribe delivers 5 messages, Upload 1 response, Chat 2 echoes + a goodbye.
		{"grpc-streaming.yaml", 5 + 1 + 3, "5 events + 1 summary + (2 echoes + goodbye)"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			s, err := runExample(t, tc.file, addr, iterations)
			if err != nil {
				t.Fatalf("%s failed against the example server: %v", tc.file, err)
			}
			if s.SuccessRate != 1 {
				t.Errorf("success rate = %v, want 1", s.SuccessRate)
			}
			if want := iterations * tc.requestsPerIter; s.Requests != want {
				t.Errorf("requests = %d, want %d (%s per iteration)", s.Requests, want, tc.why)
			}
		})
	}
}

func TestProtoFilesExampleIsForServersWithoutReflection(t *testing.T) {
	addr := startServer(t, options{reflection: false, eventInterval: defaults.eventInterval})

	// The proto_files example needs no reflection...
	s, err := runExample(t, "grpc-proto-files.yaml", addr, 3)
	if err != nil || s.SuccessRate != 1 || s.Requests != 6 {
		t.Fatalf("grpc-proto-files.yaml without reflection: %+v, %v", s, err)
	}

	// ...while the reflection-based one can't learn the schema, and says so.
	if _, err := runExample(t, "grpc-basic.yaml", addr, 1); err == nil || !strings.Contains(err.Error(), "doesn't offer reflection") {
		t.Fatalf("grpc-basic.yaml against a server without reflection: want a clear reflection error, got %v", err)
	}
}

func newGen(t *testing.T, addr string, calls ...generator.GRPCCall) *generator.GRPCGenerator {
	t.Helper()
	opts := generator.DefaultGRPCOptions()
	opts.Plaintext = true
	opts.Timeout = 3 * time.Second
	g, err := generator.NewGRPCGenerator(generator.GRPCTarget{Address: addr, Calls: calls}, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func TestUserServiceBehaviour(t *testing.T) {
	addr := startServer(t, defaults)
	const svc = "acme.user.v1.UserService/"

	// A created user can be read back, with the id and a creation time.
	g := newGen(t, addr,
		generator.GRPCCall{Method: svc + "CreateUser", Message: `{"name":"alice","age":30}`, Extract: map[string]string{"id": "json:id"},
			ExpectBody: map[string]string{"json:name": "alice", "json:age": "30", "json:createdAt": ""}},
		generator.GRPCCall{Method: svc + "GetUser", Message: `{"id":"{{.Vars.id}}"}`, ExpectBody: map[string]string{"json:name": "alice"}})
	for i, r := range g.Do(context.Background(), 0) {
		if !r.Success() {
			t.Fatalf("call %d failed: %+v", i, r)
		}
	}

	// The error paths the examples can use expect_code against.
	for name, call := range map[string]generator.GRPCCall{
		"unknown user": {Method: svc + "GetUser", Message: `{"id":"nobody"}`, ExpectCode: []string{"NOT_FOUND"}},
		"missing name": {Method: svc + "CreateUser", Message: `{"age":5}`, ExpectCode: []string{"INVALID_ARGUMENT"}},
		"negative age": {Method: svc + "CreateUser", Message: `{"name":"x","age":-1}`, ExpectCode: []string{"INVALID_ARGUMENT"}},
	} {
		for _, r := range newGen(t, addr, call).Do(context.Background(), 0) {
			if !r.Success() {
				t.Errorf("%s: %+v", name, r)
			}
		}
	}
}

func TestFeedServiceBehaviour(t *testing.T) {
	addr := startServer(t, defaults)
	const svc = "acme.feed.v1.Feed/"

	t.Run("subscribe streams events for the topic until the client stops", func(t *testing.T) {
		g := newGen(t, addr, generator.GRPCCall{Method: svc + "Subscribe", Message: `{"topic":"orders"}`, Receive: 4,
			ExpectBody: map[string]string{"json:topic": "orders"}})
		if res := g.Do(context.Background(), 0); len(res) != 4 {
			t.Fatalf("want 4 events, got %d: %+v", len(res), res)
		}
	})

	t.Run("subscribe requires a topic", func(t *testing.T) {
		g := newGen(t, addr, generator.GRPCCall{Method: svc + "Subscribe", Message: `{}`, ExpectCode: []string{"INVALID_ARGUMENT"}})
		for _, r := range g.Do(context.Background(), 0) {
			if !r.Success() {
				t.Fatalf("%+v", r)
			}
		}
	})

	t.Run("upload counts what it was sent", func(t *testing.T) {
		g := newGen(t, addr, generator.GRPCCall{Method: svc + "Upload", Messages: []string{`{"id":"a","payload":"12345"}`}, Repeat: 7,
			ExpectBody: map[string]string{"json:accepted": "7", "json:totalBytes": "35"}})
		for _, r := range g.Do(context.Background(), 0) {
			if !r.Success() {
				t.Fatalf("%+v", r)
			}
		}
	})

	t.Run("chat echoes every message then says goodbye", func(t *testing.T) {
		g := newGen(t, addr, generator.GRPCCall{Method: svc + "Chat", Messages: []string{`{"text":"a"}`, `{"text":"b"}`, `{"text":"c"}`}})
		res := g.Do(context.Background(), 0)
		if len(res) != 4 { // 3 echoes + goodbye
			t.Fatalf("want 3 echoes + goodbye, got %d: %+v", len(res), res)
		}
	})
}

func TestNewServerRegistersBothServices(t *testing.T) {
	srv, err := newServer(defaults)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	info := srv.GetServiceInfo()
	for _, want := range []string{"acme.user.v1.UserService", "acme.feed.v1.Feed"} {
		if _, ok := info[want]; !ok {
			t.Errorf("service %s not registered; have %v", want, info)
		}
	}
}

// logged starts a server whose log goes to a buffer, and returns both.
func logged(t *testing.T, opts options) (addr string, logs func() string) {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	opts.logger = log.New(&lockedWriter{mu: &mu, w: &buf}, "", 0)
	addr = startServer(t, opts)
	return addr, func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// waitFor polls until cond holds: the "done" line is written by the server
// just after the client has its answer, so a check right away could race it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestEveryMethodIsLoggedWhenCalledAndWhenDone(t *testing.T) {
	opts := defaults
	opts.logRequests = true
	addr, logs := logged(t, opts)
	const user, feed = "acme.user.v1.UserService/", "acme.feed.v1.Feed/"

	// One call of every method (reflection supplies the schema, which is
	// itself logged, as a call to the reflection service).
	g := newGen(t, addr,
		generator.GRPCCall{Method: user + "CreateUser", Message: `{"name":"alice","age":30}`},
		generator.GRPCCall{Method: user + "GetUser", Message: `{"id":"nobody"}`, ExpectCode: []string{"NOT_FOUND"}},
		generator.GRPCCall{Method: feed + "Subscribe", Message: `{"topic":"orders"}`, Receive: 2},
		generator.GRPCCall{Method: feed + "Upload", Messages: []string{`{"id":"a","payload":"xy"}`}, Repeat: 3},
		generator.GRPCCall{Method: feed + "Chat", Messages: []string{`{"text":"hi"}`}})
	for _, r := range g.Do(context.Background(), 0) {
		if !r.Success() {
			t.Fatalf("%+v", r)
		}
	}

	kinds := map[string]string{
		"CreateUser": "unary", "GetUser": "unary",
		"Subscribe": "server-streaming", "Upload": "client-streaming", "Chat": "bidi-streaming",
	}
	for method, kind := range kinds {
		full := user + method
		if method == "Subscribe" || method == "Upload" || method == "Chat" {
			full = feed + method
		}
		waitFor(t, method+" to be logged as done", func() bool { return strings.Contains(logs(), "done "+full) })
		out := logs()
		if !strings.Contains(out, "call "+full) || !strings.Contains(out, kind) {
			t.Errorf("no 'call' line naming %s as %s in:\n%s", full, kind, out)
		}
		if strings.Index(out, "call "+full) > strings.Index(out, "done "+full) {
			t.Errorf("%s: the call line must come before its done line", full)
		}
	}

	out := logs()
	for _, want := range []string{
		"done acme.user.v1.UserService/CreateUser", "OK",
		"done acme.user.v1.UserService/GetUser", "NOT_FOUND", `err="user \"nobody\" not found"`,
		"in=3 out=1", // Upload: three items in, one summary out
		"in=1 out=2", // Chat: one message in; the echo and the goodbye out
		"from=127.0.0.1:",
		"user created user-1 name=\"alice\" age=30",
		"upload received 3 items, 6 payload bytes",
		"chat client finished sending; saying goodbye after 1 messages",
		"grpc.reflection.v1.ServerReflection/ServerReflectionInfo", // reflection calls are methods too
		"conn connected",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q:\n%s", want, out)
		}
	}
}

func TestRequestLoggingCanBeTurnedOffLeavingASummary(t *testing.T) {
	opts := defaults
	opts.logRequests = false
	srv, err := newServer(func() options {
		var buf bytes.Buffer
		opts.logger = log.New(&buf, "", 0)
		return opts
	}())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)

	g := newGen(t, lis.Addr().String(), generator.GRPCCall{Method: "acme.user.v1.UserService/CreateUser", Message: `{"name":"bob"}`})
	g.Do(context.Background(), 0)
	g.Do(context.Background(), 0)

	// No per-request lines were written (checked below via the counters),
	// but the summary still accounts for the traffic, then resets.
	waitFor(t, "RPCs to be counted", func() bool { n, _ := srv.rpcs.totals(); return n >= 2 })
	line, ok := srv.rpcs.summary(10 * time.Second)
	if !ok || !strings.Contains(line, "CreateUser=2") || !strings.Contains(line, "(0 failed)") {
		t.Fatalf("summary = %q, %v", line, ok)
	}
	if _, ok := srv.rpcs.summary(10 * time.Second); ok {
		t.Fatal("an idle window must produce no summary line")
	}
}

// A server-side error status counts as a failed RPC in the summary; a client
// hanging up on a stream does not — that is how a bounded stream normally ends.
func TestServerErrorsAreCountedButClientHangupsAreNot(t *testing.T) {
	srv, err := newServer(defaults)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)

	g := newGen(t, lis.Addr().String(),
		generator.GRPCCall{Method: "acme.user.v1.UserService/GetUser", Message: `{"id":"nope"}`, ExpectCode: []string{"NOT_FOUND"}},
		// resonate cancels this stream after one event: a hang-up, not a failure.
		generator.GRPCCall{Method: "acme.feed.v1.Feed/Subscribe", Message: `{"topic":"t"}`, Receive: 1})
	for _, r := range g.Do(context.Background(), 0) {
		if !r.Success() {
			t.Fatalf("%+v", r)
		}
	}

	var line string
	waitFor(t, "both RPCs to be counted", func() bool {
		l, ok := srv.rpcs.summary(time.Second)
		if ok {
			line += l + "\n"
		}
		return strings.Contains(line, "GetUser=1") && strings.Contains(line, "Subscribe=1")
	})
	if !strings.Contains(line, "(1 failed)") || strings.Contains(line, "(2 failed)") {
		t.Fatalf("want exactly the NOT_FOUND counted as failed, got:\n%s", line)
	}
}
