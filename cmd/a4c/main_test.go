package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

// TestRootCommandSurface pins the root CLI surface to the intended minimal set.
// If this fails, either a command was added at root (it should be a subcommand
// of task or status) or the dispatch table was changed without updating the
// documented surface.
func TestRootCommandSurface(t *testing.T) {
	want := []string{"init", "install", "task", "mcp", "status"}
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

// TestHostStartCommands pins the launch lines printed by init. They must stay
// in sync with the README's per-host sections.
func TestHostStartCommands(t *testing.T) {
	cmds := hostStartCommands()
	want := map[string]hostStartCommand{
		"Copilot":     {host: "Copilot", command: "copilot --agent a4c"},
		"OpenCode":    {host: "OpenCode", command: "opencode", hint: "(then /agents to select a4c)"},
		"Antigravity": {host: "Antigravity", command: "agy", hint: "(then type /a4c — a skill, not an agent)"},
	}
	if len(cmds) != len(want) {
		t.Fatalf("expected %d host start commands, got %d", len(want), len(cmds))
	}
	for _, c := range cmds {
		if c.host == "" || c.command == "" {
			t.Errorf("empty host start entry: %+v", c)
		}
		w, ok := want[c.host]
		if !ok {
			t.Errorf("unexpected host %q", c.host)
			continue
		}
		if c.command != w.command || c.hint != w.hint {
			t.Errorf("%s = {%q %q}, want {%q %q}", c.host, c.command, c.hint, w.command, w.hint)
		}
	}
}

// TestBannerLegend pins the three legend lines beside the wordmark: line 1 is
// the product name, line 3 is the release version, so the banner always tells
// the operator which binary they are running.
func TestBannerLegend(t *testing.T) {
	if bannerLegend[0] != "Agent for Coding" {
		t.Errorf("banner line 1 = %q, want %q", bannerLegend[0], "Agent for Coding")
	}
	if bannerLegend[1] != "Knowledge Building" {
		t.Errorf("banner line 2 = %q, want %q", bannerLegend[1], "Knowledge Building")
	}
	if want := "v. " + a4c.Version; bannerLegend[2] != want {
		t.Errorf("banner line 3 = %q, want %q", bannerLegend[2], want)
	}

	// The rendered banner must carry the version on the third line.
	out := banner()
	if !strings.Contains(out, "v. "+a4c.Version) {
		t.Errorf("rendered banner does not print the version:\n%s", out)
	}
}

// TestVersionLine pins the exact shape of `a4c --version` output: a single
// "a4c <semver>" line that scripts can parse.
func TestVersionLine(t *testing.T) {
	got := versionLine()
	if !strings.HasPrefix(got, "a4c ") {
		t.Fatalf("versionLine() = %q, want prefix %q", got, "a4c ")
	}
	version := strings.TrimPrefix(got, "a4c ")
	if _, err := a4c.BumpPatch(version); err != nil {
		t.Fatalf("versionLine() reports %q, which is not a major.minor.patch version: %v", version, err)
	}
}

// Every accepted spelling of the version request must print the same line and
// never reach command dispatch, which would reject them as unknown commands.
func TestVersionSpellingsHandledBeforeDispatch(t *testing.T) {
	want := versionLine()
	for _, arg := range []string{"--version", "-version", "version"} {
		oldArgs := os.Args
		os.Args = []string{"a4c", arg}
		out, errOut := captureOutput(t, func() { main() })
		os.Args = oldArgs
		if strings.TrimSpace(out) != want {
			t.Errorf("a4c %s printed %q (stderr %q), want %q", arg, out, errOut, want)
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
