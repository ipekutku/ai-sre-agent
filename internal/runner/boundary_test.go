package runner

import (
	"os/exec"
	"strings"
	"testing"
)

// Execution collects observations; evaluation alone owns the expected answer.
// This checks imports only: it cannot detect a direct read of ground-truth.yaml.
func TestRunnerCannotImportEvaluation(t *testing.T) {
	const forbidden = "github.com/ipekutku/ai-sre-agent/internal/evaluation"
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps .: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == forbidden || strings.HasPrefix(dep, forbidden+"/") {
			t.Errorf("runner depends on %s", dep)
		}
	}
}
