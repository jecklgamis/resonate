package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func writeTempScenario(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing temp scenario: %v", err)
	}
	return path
}

func wsEchoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		for {
			typ, msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			resp, _ := json.Marshal(map[string]string{"echo": string(msg)})
			if err := conn.Write(ctx, typ, resp); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunCommandWSScenarioEndToEnd(t *testing.T) {
	t.Chdir(t.TempDir()) // runAndReport now writes report.html/report.json by default; keep the repo clean
	srv := wsEchoServer(t)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	path := writeTempScenario(t, `
protocol: ws

load:
  requests: 3
  workers: 1

ws:
  url: `+wsURL+`
  messages:
    - body: '{"hello":"{{.Seq}}"}'
      wait: true
      extract:
        echoed: json:echo
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--json"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})

	var summary struct {
		Requests    int     `json:"requests"`
		SuccessRate float64 `json:"success_rate"`
	}
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("output isn't valid JSON: %v\noutput: %s", err, out)
	}
	// 3 iterations * (1 connect + 1 message) = 6 reported results.
	if summary.Requests != 6 {
		t.Errorf("Requests = %d, want 6 (3 iterations * (connect + 1 message))", summary.Requests)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
}

func TestRunCommandWSExpectBodyMismatchFailsEveryIteration(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := wsEchoServer(t)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	path := writeTempScenario(t, `
protocol: ws

load:
  requests: 3
  workers: 1

ws:
  url: `+wsURL+`
  messages:
    - body: 'hello'
      wait: true
      expect_body:
        json:echo: "goodbye"
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--json"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})

	var summary struct {
		SuccessRate float64 `json:"success_rate"`
	}
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("output isn't valid JSON: %v\noutput: %s", err, out)
	}
	if summary.SuccessRate != 0.5 {
		t.Errorf("SuccessRate = %v, want 0.5 (connects all succeed; every message fails the json:echo check)", summary.SuccessRate)
	}
}
