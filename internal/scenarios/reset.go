package scenarios

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// resetServices are recreated by a reset: the demo services, whose fault state
// is in memory, and the backends, whose data lives in the container filesystem
// (Tempo, Loki) or an anonymous volume (Prometheus). Grafana keeps no telemetry
// and is left running.
var resetServices = []string{"inventory-api", "checkout-api", "prometheus", "tempo", "loki"}

// maxResetOutput bounds the compose output kept in an error.
const maxResetOutput = 2048

// ComposeReset recreates the environment's containers with docker compose, so
// a run cannot see an earlier run's metrics, traces, logs or fault state.
// The command is fixed apart from the operator-supplied compose file.
type ComposeReset struct {
	File string
}

// Reset recreates the containers and returns once compose has started them;
// the caller waits for readiness. Images are not rebuilt, so a suite runs the
// code that `make up` built.
func (c ComposeReset) Reset(ctx context.Context) error {
	if c.File == "" {
		return fmt.Errorf("compose file is required")
	}
	cmd := exec.CommandContext(ctx, "docker", c.args()...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// Bound the wait for output pipes if compose is killed on cancellation.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("docker compose up: %w", ctx.Err())
		}
		return fmt.Errorf("docker compose up: %w: %s", err, tail(out.String(), maxResetOutput))
	}
	return nil
}

func (c ComposeReset) args() []string {
	return append([]string{"compose", "-f", c.File, "up", "--detach", "--no-build",
		"--force-recreate", "--renew-anon-volumes"}, resetServices...)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}
