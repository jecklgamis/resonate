package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jecklgamis/resonate/internal/config"
)

// Every complete `protocol:` scenario in the multiple-scenarios guide, and the
// shipped example, is loaded the way `resonate run` loads it (strictly, for a
// file with scenarios:, so a misspelled key fails here instead of being
// ignored), resolved, and — unless marked parse-only — dry-run against a local
// server, which builds every scenario and sends one iteration of each.
func TestMultipleScenariosGuideAndExampleAreValidAndRun(t *testing.T) {
	// Read everything first: running a scenario changes the working directory.
	md, err := os.ReadFile("../../docs/multiple-scenarios.md")
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../../examples/http-scenarios.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fence := regexp.MustCompile("(?s)(<!-- docs-test: parse-only -->\n)?```yaml\n(.*?)```")
	blocks := []docBlock{{src: string(example)}}
	for _, m := range fence.FindAllStringSubmatch(string(md), -1) {
		if strings.HasPrefix(strings.TrimSpace(m[2]), "protocol:") {
			blocks = append(blocks, docBlock{src: m[2], parseOnly: m[1] != ""})
		}
	}

	t.Chdir(t.TempDir()) // runs write report files to the working directory
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"7"}`))
	}))
	t.Cleanup(srv.Close)

	ran := 0
	for i, blk := range blocks {
		src := strings.ReplaceAll(blk.src, "http://localhost:8080", srv.URL)
		path := writeTempScenario(t, src)

		sc, err := config.Load(path) // strict for scenarios files
		if err != nil {
			t.Errorf("block %d doesn't load: %v\n%s", i, err, blk.src)
			continue
		}
		if len(sc.Scenarios) == 0 {
			t.Errorf("block %d has no scenarios:\n%s", i, blk.src)
			continue
		}
		if _, err := sc.Resolve(); err != nil {
			t.Errorf("block %d doesn't resolve: %v\n%s", i, err, blk.src)
			continue
		}
		if blk.parseOnly {
			continue
		}
		ran++
		mu.Lock()
		before := hits
		mu.Unlock()
		cmd := newRunCommand()
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		cmd.SetArgs([]string{path, "--dry-run", "--quiet"})
		var runErr error
		captureStdout(t, func() { captureStderr(t, func() { runErr = cmd.Execute() }) })
		if runErr != nil {
			t.Errorf("block %d failed a dry run: %v\n%s", i, runErr, blk.src)
		}
		mu.Lock()
		sent := hits - before
		mu.Unlock()
		if sent < len(sc.Scenarios) {
			t.Errorf("block %d: a dry run sent %d request(s) for %d scenario(s)", i, sent, len(sc.Scenarios))
		}
	}
	if len(blocks) < 5 || ran < 5 {
		t.Fatalf("found %d scenarios and ran %d; expected at least 5 of each (the guide's four plus the example)", len(blocks), ran)
	}
}
