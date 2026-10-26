package scenarios

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeDocker puts a docker script first on PATH that records its arguments
// one per line and then runs body.
func fakeDocker(t *testing.T, body string) (argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake docker is a shell script")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argsFile + "'\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func TestComposeResetRunsFixedCommand(t *testing.T) {
	argsFile := fakeDocker(t, "exit 0")
	if err := (ComposeReset{File: "deploy/docker compose.yml"}).Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	// The file path stays one argument even with a space: no shell is involved.
	want := []string{"compose", "-f", "deploy/docker compose.yml", "up", "--detach", "--no-build",
		"--force-recreate", "--renew-anon-volumes", "inventory-api", "checkout-api", "prometheus", "tempo", "loki"}
	if strings.Join(want, "\n")+"\n" != string(got) {
		t.Errorf("docker args:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

func TestComposeResetReportsBoundedOutput(t *testing.T) {
	fakeDocker(t, "printf 'early line\\n'; head -c 5000 /dev/zero | tr '\\0' x; printf '\\nno such image\\n' >&2; exit 3")
	err := (ComposeReset{File: "compose.yml"}).Reset(context.Background())
	if err == nil {
		t.Fatal("Reset succeeded, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exit status 3") || !strings.Contains(msg, "no such image") {
		t.Errorf("error lacks exit status or compose output: %.200s", msg)
	}
	if strings.Contains(msg, "early line") || len(msg) > maxResetOutput+200 {
		t.Errorf("error output not bounded: %d bytes", len(msg))
	}
}

func TestComposeResetCancellation(t *testing.T) {
	fakeDocker(t, "exec sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := (ComposeReset{File: "compose.yml"}).Reset(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Reset = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("Reset took %s after cancellation", time.Since(start))
	}
}

func TestComposeResetRequiresFile(t *testing.T) {
	argsFile := fakeDocker(t, "exit 0")
	if err := (ComposeReset{}).Reset(context.Background()); err == nil {
		t.Error("Reset without compose file succeeded")
	}
	if _, err := os.Stat(argsFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("docker ran without a compose file: %v", err)
	}
}
