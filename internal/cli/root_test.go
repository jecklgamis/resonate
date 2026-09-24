package cli

import (
	"os"
	"strings"
	"testing"
)

func TestNewRootCommandHasHitAndRunSubcommands(t *testing.T) {
	root := NewRootCommand("dev")
	names := map[string]bool{}
	for _, c := range root.Commands() {
		names[c.Name()] = true
	}
	if !names["hit"] {
		t.Error("root command tree is missing \"hit\"")
	}
	if !names["run"] {
		t.Error("root command tree is missing \"run\"")
	}
}

func TestNewRootCommandVersion(t *testing.T) {
	root := NewRootCommand("1.2.3")
	if root.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", root.Version, "1.2.3")
	}
}

func TestExecuteRunsRootCommand(t *testing.T) {
	// Execute reads os.Args internally (it doesn't take args), so pin it to
	// something harmless (--help) for the duration of the test rather than
	// letting it pick up `go test`'s own flags.
	origArgs := os.Args
	os.Args = []string{"resonate", "--help"}
	t.Cleanup(func() { os.Args = origArgs })

	stdout := captureStdout(t, func() {
		if err := Execute("dev"); err != nil {
			t.Errorf("Execute error: %v", err)
		}
	})
	if !strings.Contains(stdout, "resonate") {
		t.Errorf("stdout = %q, want it to mention \"resonate\" (root command help)", stdout)
	}
}
