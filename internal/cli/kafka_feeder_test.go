package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
