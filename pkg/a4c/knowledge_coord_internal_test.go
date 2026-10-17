package a4c

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/a4c/pkg/a4c/knowledge"
)

// TestKnowledgeCoord_TokenIssuedOnlyWhenEnabled verifies the opt-in contract at
// the coordinator boundary: an enabled workspace yields a per-activity
// execution-context token on the session config; a disabled one yields none.
func TestKnowledgeCoord_TokenIssuedOnlyWhenEnabled(t *testing.T) {
	run := func(t *testing.T, enabled bool) string {
		t.Helper()
		dir := t.TempDir()
		ws := filepath.Join(dir, "repo")
		_ = os.MkdirAll(ws, 0o755)
		_ = os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644)
		_ = os.WriteFile(filepath.Join(ws, "x_test.go"), []byte("package x\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n"), 0o644)
		if enabled {
			if err := knowledge.WriteConfig(filepath.Join(dir), knowledge.Config{Enabled: true, Project: "skiff"}); err != nil {
				t.Fatal(err)
			}
		}
		store, _ := NewDurableActivityStore(filepath.Join(dir, "activities"))
		memory, _ := NewDurableMemoryStore(filepath.Join(dir, "memory"))
		roster, _ := DefaultTeamRoster(filepath.Join(dir, "roster"))
		gov := NewPolicyGovernor([]string{"governed_read_file", "governed_propose_activity", "governed_submit_review", "governed_request_human_decision", "governed_propose_memory", "governed_apply_patch"})
		journal, _ := NewDurableIntentJournal(filepath.Join(dir, "journal"))
		fake := NewFakeProvider()
		coord := NewTeamCoordinator(ws, store, roster, memory, gov, journal, &NativeGoTestRunner{}, fake)

		var tok string
		fake.SessionResponseFunc = func(ctx context.Context, session *FakeSession, prompt string) (*AgentMessage, error) {
			tok = session.Config().KnowledgeToken
			if session.Config().AgentID == "lead" {
				if session.Config().OnProposeActivity != nil {
					session.Config().OnProposeActivity(ActivityProposal{ProposalID: "c1", ProposedAgentID: "coder", Objective: "impl", RequestedBudget: Budget{Allocated: 100000}})
				}
				return &AgentMessage{Content: "delegated"}, nil
			}
			_ = os.WriteFile(filepath.Join(ws, "calc.go"), []byte("package x\n"), 0o644)
			return &AgentMessage{Content: "done"}, nil
		}

		// Wire the knowledge service exactly as buildService does.
		if ks, err := knowledge.NewService(dir, func(activityID string) bool {
			rec, err := store.Get(activityID)
			return err == nil && rec != nil && rec.Status == StatusClaimed
		}); err == nil {
			coord.SetKnowledge(ks)
			defer ks.Close()
		} else if enabled {
			t.Fatalf("knowledge service should open when enabled: %v", err)
		}

		_, _ = coord.SubmitTask("implement", Budget{Allocated: 300000}, nil)
		// Run enough steps for lead + coder.
		_, _ = coord.RunLoop(context.Background(), 6)
		return tok
	}

	t.Run("enabled", func(t *testing.T) {
		if tok := run(t, true); tok == "" {
			t.Fatalf("enabled workspace must issue a non-empty KnowledgeToken")
		}
	})
	t.Run("disabled", func(t *testing.T) {
		if tok := run(t, false); tok != "" {
			t.Fatalf("disabled workspace must not issue a KnowledgeToken, got %q", tok)
		}
	})
}
