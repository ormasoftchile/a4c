package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestRootCommandSurface pins the root CLI surface to the intended minimal set.
// If this fails, either a command was added at root (it should be a subcommand
// of task or status) or the dispatch table was changed without updating the
// documented surface.
func TestRootCommandSurface(t *testing.T) {
	want := []string{"init", "install", "task", "mcp", "status", "demo"}
	got := make([]string, 0, len(rootCommands))
	for _, c := range rootCommands {
		if c.name == "" {
			t.Errorf("root command with empty name")
		}
		if c.summary == "" {
			t.Errorf("root command %q has no summary; printUsage would render a blank line", c.name)
		}
		if c.run == nil {
			t.Errorf("root command %q has no handler", c.name)
		}
		got = append(got, c.name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("root surface = %v, want %v", got, want)
	}
}

// TestRootCommandNamesUnique guards the lookup table against collisions, which
// would make dispatch silently pick the first match.
func TestRootCommandNamesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range rootCommands {
		if seen[c.name] {
			t.Errorf("duplicate root command %q", c.name)
		}
		seen[c.name] = true
	}
}

// TestRemovedCommandsRedirectToLiveTargets: every displaced command must name a
// destination that actually exists, or the hard break becomes a dead end.
func TestRemovedCommandsRedirectToLiveTargets(t *testing.T) {
	if len(removedCommands) == 0 {
		t.Fatal("expected displaced commands to be documented")
	}
	for old, target := range removedCommands {
		if old == "" {
			t.Errorf("empty key in removedCommands")
		}
		if !strings.HasPrefix(target, "a4c ") {
			t.Errorf("removed command %q target %q is not an a4c invocation", old, target)
		}
		// The target's root element must still dispatch.
		root := strings.Fields(target)
		if len(root) < 2 {
			t.Errorf("target %q for %q names no subcommand", target, old)
			continue
		}
		if _, ok := lookupRoot(root[1]); !ok {
			t.Errorf("removed command %q points at root %q, which is not a root command", old, root[1])
		}
	}
	// Every displaced name must be absent from the root surface.
	for old := range removedCommands {
		if _, ok := lookupRoot(old); ok {
			t.Errorf("%q is listed as removed but is still a root command", old)
		}
	}
}

// TestSuggestCommand covers the typo path of the hard break.
func TestSuggestCommand(t *testing.T) {
	cases := map[string]string{
		"stauts":  "status",
		"tsak":    "task",
		"instal":  "install",
		"mmcp":    "mcp",
		"init2":   "init", // close enough to suggest
		"totally": "",     // too far to suggest
	}
	for input, want := range cases {
		if got := suggestCommand(input); got != want {
			t.Errorf("suggestCommand(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"status", "status", 0},
		{"stauts", "status", 2},
		{"cat", "cut", 1},
		{"", "abc", 3},
	}
	for _, tc := range cases {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
