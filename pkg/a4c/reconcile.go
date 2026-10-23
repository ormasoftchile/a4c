package a4c

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// LocalEffectReconciler reconciles journal intents whose effects are local to
// the workspace (for example an applied patch that now exists on disk).
type LocalEffectReconciler struct {
	WorkspaceDir string
}

func (r *LocalEffectReconciler) Reconcile(ctx context.Context, intent IntentRecord) (bool, string, error) {
	if intent.Action == "apply_patch" {
		target := filepath.Join(r.WorkspaceDir, intent.Target)
		if _, err := os.Stat(target); err == nil {
			return true, fmt.Sprintf("local-file-%s", intent.Target), nil
		}
	}
	return false, "", nil
}

func parseStringMap(val any) map[string]string {
	out := map[string]string{}
	m, ok := val.(map[string]any)
	if !ok {
		return out
	}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
