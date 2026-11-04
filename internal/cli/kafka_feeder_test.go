package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jecklgamis/resonate/internal/config"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

func kafkaWithMessages(t *testing.T, topic string, values ...string) string {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	addr := c.ListenAddrs()[0]
	cl, err := kgo.NewClient(kgo.SeedBrokers(addr))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for _, v := range values {
		if err := cl.ProduceSync(context.Background(), &kgo.Record{Topic: topic, Value: []byte(v)}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
	return addr
}

func TestRunCommandKafkaFeederDrivesRequests(t *testing.T) {
	t.Chdir(t.TempDir())
	addr := kafkaWithMessages(t, "users", `{"email":"alice@example.com"}`, `{"email":"bob@example.com"}`)

	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Query().Get("email")+"/"+r.URL.Query().Get("off"))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 5
  workers: 1
http:
  feeder:
    kafka:
      brokers: ["`+addr+`"]
      topic: users
  targets:
    - url: `+srv.URL+`?email={{.Feeder.email}}&off={{.Feeder._offset}}
`)
	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})
	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	want := "alice@example.com/0,bob@example.com/1,alice@example.com/0,bob@example.com/1,alice@example.com/0"
	if g := strings.Join(got, ","); g != want {
		t.Errorf("requests = %s\nwant       %s", g, want)
	}
}

func TestRunCommandKafkaFeederConfigErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("RESONATE_TEST_UNSET_PW", "")
	cases := map[string]struct{ feeder, want string }{
		"file and kafka": {"    file: a.csv\n    kafka: {brokers: [\"127.0.0.1:1\"], topic: t}", "either file or kafka"},
		"mode":           {"    mode: random\n    kafka: {brokers: [\"127.0.0.1:1\"], topic: t}", "mode applies to file feeders"},
		"password both":  {"    kafka: {brokers: [\"127.0.0.1:1\"], topic: t, sasl_mechanism: PLAIN, username: u, password: p, password_env: X}", "either password or password_env"},
		"password unset": {"    kafka: {brokers: [\"127.0.0.1:1\"], topic: t, sasl_mechanism: PLAIN, username: u, password_env: RESONATE_NOT_SET_ANYWHERE}", "is not set"},
		"bad timeout":    {"    kafka: {brokers: [\"127.0.0.1:1\"], topic: t, timeout: soon}", "invalid timeout"},
		"no topic":       {"    kafka: {brokers: [\"127.0.0.1:1\"]}", "topic is required"},
		"unreachable":    {"    kafka: {brokers: [\"127.0.0.1:1\"], topic: t, timeout: 1s}", "can't reach brokers"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTempScenario(t, "protocol: http\nload: {requests: 1, workers: 1}\nhttp:\n  feeder:\n"+tc.feeder+"\n  targets:\n    - url: http://localhost:1/\n")
			cmd := newRunCommand()
			cmd.SetArgs([]string{path, "--quiet"})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "http.feeder") {
				t.Errorf("error = %v, want http.feeder ... %q", err, tc.want)
			}
		})
	}
}

// Every complete `protocol:` scenario in the Kafka feeder guide is strict-parsed
// (a misspelled key would otherwise be silently ignored by resonate itself) and,
// unless marked parse-only, run against an in-process broker and HTTP server with
// only the addresses and the load swapped. The shipped example gets the same.
func TestKafkaFeederGuideAndExampleAreValidAndRun(t *testing.T) {
	// Read everything first: running a scenario changes the working directory.
	md, err := os.ReadFile("../../docs/kafka-feeder.md")
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../../examples/http-kafka-feeder.yaml")
	if err != nil {
		t.Fatal(err)
	}
	blocks := []docBlock{{src: string(example)}}
	fence := regexp.MustCompile("(?s)(<!-- docs-test: parse-only -->\n)?```yaml\n(.*?)```")
	for _, m := range fence.FindAllStringSubmatch(string(md), -1) {
		if strings.HasPrefix(strings.TrimSpace(m[2]), "protocol:") {
			blocks = append(blocks, docBlock{src: m[2], parseOnly: m[1] != ""})
		}
	}

	addr := kafkaWithMessages(t, "users", `{"email":"a@example.com","password":"x","id":"1"}`, `{"email":"b@example.com","password":"y","id":"2"}`)
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, string(b))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	ran := 0
	for i, blk := range blocks {
		dec := yaml.NewDecoder(strings.NewReader(blk.src))
		dec.KnownFields(true)
		var sc config.Scenario
		if err := dec.Decode(&sc); err != nil {
			t.Errorf("block %d doesn't parse strictly: %v\n%s", i+1, err, blk.src)
			continue
		}
		if blk.parseOnly {
			continue
		}
		ran++
		sc.HTTP.Feeder.Kafka.Brokers = []string{addr}
		for j := range sc.HTTP.Targets {
			sc.HTTP.Targets[j].URL = srv.URL + "/login"
		}
		sc.Load = config.LoadConfig{Requests: 4, Workers: 1}
		b, err := yaml.Marshal(sc)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		seen = nil
		mu.Unlock()
		cmd := newRunCommand()
		cmd.SetArgs([]string{writeTempScenario(t, string(b)), "--quiet"})
		captureStdout(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Errorf("block %d failed: %v\n%s", i+1, err, blk.src)
			}
		})
		if len(seen) != 4 || !strings.Contains(seen[0], "a@example.com") || !strings.Contains(seen[1], "b@example.com") || !strings.Contains(seen[2], "a@example.com") {
			t.Errorf("block %d sent %q, want the two messages cycling", i+1, seen)
		}
	}
	if len(blocks) < 3 || ran < 2 {
		t.Fatalf("found %d scenarios and ran %d; expected the example plus the guide's (at least 3 and 2)", len(blocks), ran)
	}
}
