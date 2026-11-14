package cmd

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestRunChildProcessBackupFatalTriggersTermination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group test requires Unix")
	}

	fatal := make(chan struct{})
	close(fatal)
	sigCh := make(chan os.Signal, 1)

	child := exec.Command("sh", "-c", "sleep 30")
	start := time.Now()
	code, triggered, err := runChildProcess(child, sigCh, fatal)
	if err != nil {
		t.Fatal(err)
	}
	if !triggered {
		t.Fatalf("backup fatal did not trigger termination (code %d)", code)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("termination took too long: %v", elapsed)
	}
}
