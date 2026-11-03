package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jecklgamis/resonate/internal/config"
)

// The gRPC docs are full of scenarios people will copy and run. These tests
// keep them honest: every complete `protocol: grpc` scenario in the docs must
// parse strictly (a misspelled key is an error, where resonate itself would
// silently ignore it), and every one that targets the example server's
// services must actually run against it.

var yamlFence = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// grpcScenarios returns the complete gRPC scenarios in a docs page.
func grpcScenarios(t *testing.T, page string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", page))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range yamlFence.FindAllStringSubmatch(string(b), -1) {
		if strings.HasPrefix(strings.TrimSpace(m[1]), "protocol: grpc") {
			out = append(out, m[1])
		}
	}
	return out
}

func TestDocumentedGRPCScenariosAreValidAndRun(t *testing.T) {
	addr := startServer(t, defaults)

	total, ran := 0, 0
	for _, page := range []string{"grpc.md", "scenarios.md"} {
		for i, src := range grpcScenarios(t, page) {
			total++
			name := page + "#" + string(rune('A'+i))

			// Strict: unknown keys are errors.
			dec := yaml.NewDecoder(strings.NewReader(src))
			dec.KnownFields(true)
			var sc config.Scenario
			if err := dec.Decode(&sc); err != nil {
				t.Errorf("%s: doesn't parse strictly: %v\n%s", name, err, src)
				continue
			}

			// Run the ones aimed at the example server's services.
			if !strings.Contains(src, "acme.") {
				continue
			}
			ran++
			path := filepath.Join(t.TempDir(), "doc.yaml")
			if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			s, err := runScenario(t, path, addr, 3)
			if err != nil {
				t.Errorf("%s fails against the example server: %v\n%s", name, err, src)
				continue
			}
			if s.SuccessRate != 1 || s.Requests == 0 {
				t.Errorf("%s: %+v, want every request to succeed\n%s", name, s, src)
			}
		}
	}
	// Guard against the extraction silently finding nothing.
	if total < 5 || ran < 5 {
		t.Fatalf("found %d gRPC scenarios in the docs and ran %d; expected at least 5 of each", total, ran)
	}
}

// Every example file the guide points at must exist.
func TestGRPCGuideLinksToRealExamples(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "grpc.md"))
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`blob/main/(examples/[\w./-]+)`).FindAllStringSubmatch(string(b), -1)
	if len(refs) < 4 {
		t.Fatalf("expected the guide to link at least 4 example files, found %d", len(refs))
	}
	for _, m := range refs {
		if _, err := os.Stat(filepath.Join(repoRoot(t), m[1])); err != nil {
			t.Errorf("docs/grpc.md links %s, which doesn't exist", m[1])
		}
	}
}
