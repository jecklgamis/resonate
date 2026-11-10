package cli

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jecklgamis/resonate/internal/config"
)

func TestScenarioCompressGzipsTheRequestBody(t *testing.T) {
	var gotBody, gotEnc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEnc = r.Header.Get("Content-Encoding")
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		b, _ := io.ReadAll(zr)
		gotBody = string(b)
	}))
	defer srv.Close()

	s, err := config.Load(writeTemp(t, fmt.Sprintf(`
protocol: http
load: {iterations: 1}
http:
  targets:
    - method: POST
      url: %s
      compress: gzip
      body: '{"events": [1, 2, 3]}'
`, srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	g, err := buildGenerator(s)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if res := g.Do(context.Background(), 0)[0]; !res.Success() {
		t.Fatalf("request failed: %v", res.Error)
	}
	if gotEnc != "gzip" || gotBody != `{"events": [1, 2, 3]}` {
		t.Fatalf("server saw Content-Encoding=%q body=%q", gotEnc, gotBody)
	}
}

func TestScenarioCompressRejectsBadConfig(t *testing.T) {
	for name, c := range map[string]struct{ yaml, want string }{
		"http unknown coding": {`
protocol: http
load: {iterations: 1}
http:
  targets: [{url: "http://localhost:1/x", body: "a", compress: lz4}]
`, `invalid compress "lz4"`},
		"sse unknown coding": {`
protocol: sse
load: {iterations: 1}
sse: {url: "http://localhost:1/x", method: POST, body: "a", compress: lz4}
`, `invalid compress "lz4"`},
		"with raw_body_file": {`
protocol: http
load: {iterations: 1}
http:
  targets: [{url: "http://localhost:1/x", raw_body_file: "` + writeTemp(t, "bin") + `", compress: gzip}]
`, "mutually exclusive"},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := config.Load(writeTemp(t, c.yaml))
			if err == nil {
				_, err = buildGenerator(s)
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
