package agent

import (
	"os/exec"
	"strings"
	"testing"
)

// The agent, its tools, and the LLM layer must never be able to read scenario
// definitions or ground truth. This checks their full dependency graphs.
func TestAgentCannotImportGroundTruth(t *testing.T) {
	const module = "github.com/ipekutku/ai-sre-agent/internal/"
	forbidden := []string{module + "evaluation", module + "scenarios"}

	for _, pkg := range []string{"./", "../tools", "../llm/..."} {
		out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
		}
		for _, dep := range strings.Fields(string(out)) {
			for _, f := range forbidden {
				if dep == f || strings.HasPrefix(dep, f+"/") {
					t.Errorf("%s depends on %s", pkg, dep)
				}
			}
		}
	}
}
