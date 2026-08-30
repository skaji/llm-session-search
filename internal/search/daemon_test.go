package search

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	daemon "github.com/sevlyar/go-daemon"
)

func TestSelectDaemonOperation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		start             bool
		status            bool
		stop              bool
		restart           bool
		want              daemonOperation
		wantMutualExError bool
	}{
		{name: "none", want: daemonOperationNone},
		{name: "start", start: true, want: daemonOperationStart},
		{name: "status", status: true, want: daemonOperationStatus},
		{name: "stop", stop: true, want: daemonOperationStop},
		{name: "restart", restart: true, want: daemonOperationRestart},
		{name: "multiple", start: true, stop: true, wantMutualExError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectDaemonOperation(test.start, test.status, test.stop, test.restart)
			if test.wantMutualExError {
				if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
					t.Fatalf("selectDaemonOperation() error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("selectDaemonOperation() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestDaemonStateFindProcess(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	state := newDaemonState(dataDir)
	if process, running, err := state.findProcess(); err != nil || running || process != nil {
		t.Fatalf("findProcess() without PID file = (%v, %t, %v)", process, running, err)
	}

	if err := os.WriteFile(state.pidPath, []byte("invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if process, running, err := state.findProcess(); err != nil || running || process != nil {
		t.Fatalf("findProcess() with invalid PID file = (%v, %t, %v)", process, running, err)
	}
	if _, err := os.Stat(state.pidPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid PID file was not removed: %v", err)
	}

	if err := os.WriteFile(state.pidPath, []byte("2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if process, running, err := state.findProcess(); err != nil || running || process != nil {
		t.Fatalf("findProcess() with stale PID file = (%v, %t, %v)", process, running, err)
	}
	if _, err := os.Stat(state.pidPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale PID file was not removed: %v", err)
	}

	lock, err := daemon.CreatePidFile(state.pidPath, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Remove() })
	process, running, err := state.findProcess()
	if err != nil {
		t.Fatal(err)
	}
	if !running || process.Pid != os.Getpid() {
		t.Fatalf("findProcess() = (%v, %t), want current process", process, running)
	}
}

func TestHandleDaemonStopWhenStopped(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	result, err := handleDaemonOperation(daemonOperationStop, t.TempDir(), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if result.runServer {
		t.Fatal("stop attempted to run the server")
	}
	if stdout.String() != "llm-session-search is not running\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestHandleDaemonStatus(t *testing.T) {
	t.Parallel()

	t.Run("running", func(t *testing.T) {
		dataDir := t.TempDir()
		lock, err := daemon.CreatePidFile(filepath.Join(dataDir, "app.pid"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lock.Remove() })
		var stdout bytes.Buffer
		result, err := handleDaemonOperation(daemonOperationStatus, dataDir, &stdout)
		if err != nil {
			t.Fatal(err)
		}
		if result.runServer {
			t.Fatal("status attempted to run the server")
		}
		want := fmt.Sprintf("llm-session-search is running (pid %d)\n", os.Getpid())
		if stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	})

	t.Run("stopped", func(t *testing.T) {
		result, err := handleDaemonOperation(daemonOperationStatus, t.TempDir(), &bytes.Buffer{})
		if !errors.Is(err, errDaemonNotRunning) {
			t.Fatalf("status error = %v", err)
		}
		if result.runServer {
			t.Fatal("status attempted to run the server")
		}
	})
}

func TestDaemonReady(t *testing.T) {
	t.Parallel()

	state := newDaemonState(t.TempDir())
	readyPath := state.readyPath(os.Getpid())
	ready, err := daemonReady(readyPath, os.Getpid())
	if !errors.Is(err, os.ErrNotExist) || ready {
		t.Fatalf("daemonReady() before marker = (%t, %v)", ready, err)
	}
	if err := state.markReady(); err != nil {
		t.Fatal(err)
	}
	ready, err = daemonReady(readyPath, os.Getpid())
	if err != nil || !ready {
		t.Fatalf("daemonReady() after marker = (%t, %v)", ready, err)
	}
	ready, err = daemonReady(readyPath, os.Getpid()+1)
	if err != nil || ready {
		t.Fatalf("daemonReady() for another PID = (%t, %v)", ready, err)
	}
	if err := state.removeReady(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(readyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readiness file was not removed: %v", err)
	}
}
