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

func TestRootHelpExplainsQuickStartsAndCapabilities(t *testing.T) {
	origArgs := os.Args
	os.Args = []string{"resonate", "--help"}
	t.Cleanup(func() { os.Args = origArgs })

	stdout := captureStdout(t, func() {
		if err := Execute("dev"); err != nil {
			t.Errorf("Execute error: %v", err)
		}
	})
	for _, want := range []string{
		"resonate hit https://example.com --duration 10s",
		"resonate run scenario.yaml",
		"WebSocket tests",
		"--dry-run",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("root help missing %q", want)
		}
	}
}

func TestCommandsWithoutRequiredArgumentsExplainHowToContinue(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "hit", args: []string{"hit"}, want: "resonate hit https://example.com --duration 10s"},
		{name: "run", args: []string{"run"}, want: "resonate run scenario.yaml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := NewRootCommand("dev")
			root.SetArgs(test.args)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Execute() error = %v, want guidance containing %q", err, test.want)
			}
		})
	}
}
