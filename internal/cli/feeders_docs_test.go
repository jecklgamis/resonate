package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jecklgamis/resonate/internal/config"
)

// Every complete `protocol:` scenario in the feeders guide is strict-parsed (a
// misspelled key would otherwise be silently ignored by resonate itself) and,
// unless marked parse-only, run against a local server with the feeder's file
// swapped for a fixture of the same shape. The quick start must hand out the
// rows in file order, as the guide says.
func TestFeedersGuideScenariosAreValidAndRun(t *testing.T) {
	// Read the page first: running a scenario changes the working directory.
	md, err := os.ReadFile("../../docs/feeders.md")
	if err != nil {
		t.Fatal(err)
	}
	fence := regexp.MustCompile("(?s)(<!-- docs-test: parse-only -->\n)?```yaml\n(.*?)```")
	var blocks []docBlock
	for _, m := range fence.FindAllStringSubmatch(string(md), -1) {
		if strings.HasPrefix(strings.TrimSpace(m[2]), "protocol:") {
			blocks = append(blocks, docBlock{src: m[2], parseOnly: m[1] != ""})
		}
	}

	t.Chdir(t.TempDir()) // runs write report files to the working directory
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "users.csv")
	jsonPath := filepath.Join(dir, "users.json")
	if err := os.WriteFile(csvPath, []byte("email,password,name\nalice@example.com,p1,alice\nbob@example.com,p2,bob\ncarol@example.com,p3,carol\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonPath, []byte(`[{"email":"alice@example.com","password":"p1","name":"alice"},{"email":"bob@example.com","password":"p2","name":"bob"},{"email":"carol@example.com","password":"p3","name":"carol"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b)+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"7"}`))
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
		if strings.HasSuffix(sc.HTTP.Feeder.File, ".csv") {
			sc.HTTP.Feeder.File = csvPath
		} else {
			sc.HTTP.Feeder.File = jsonPath
		}
		for j := range sc.HTTP.Targets {
			sc.HTTP.Targets[j].URL = srv.URL + "/login"
		}
		for j := range sc.HTTP.Flow {
			sc.HTTP.Flow[j].URL = srv.URL + "/orders/{{.Vars.order_id}}"
		}
		sc.Load = config.LoadConfig{Requests: 4, Workers: 1}
		out, err := yaml.Marshal(sc)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		bodies = nil
		mu.Unlock()
		cmd := newRunCommand()
		cmd.SetArgs([]string{writeTempScenario(t, string(out)), "--quiet"})
		captureStdout(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Errorf("block %d failed: %v\n%s", i+1, err, blk.src)
			}
		})
		mu.Lock()
		got := append([]string(nil), bodies...)
		mu.Unlock()
		if len(got) < 4 {
			t.Errorf("block %d sent %d requests, want at least 4", i+1, len(got))
			continue
		}
		if i == 0 { // the quick start: sequential, in file order, wrapping
			for k, want := range []string{"alice", "bob", "carol", "alice"} {
				if !strings.Contains(got[k], want+"@example.com") {
					t.Errorf("quick start request %d = %q, want it to use %s", k+1, got[k], want)
				}
			}
		}
	}
	if len(blocks) < 5 || ran < 2 {
		t.Fatalf("found %d scenarios and ran %d; expected at least 5 and 2", len(blocks), ran)
	}
}
