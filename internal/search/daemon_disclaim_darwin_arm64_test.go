package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestNeedsDaemonDisclaim(t *testing.T) {
	for _, tc := range []struct {
		version       string
		want, invalid bool
	}{
		{"15.7", false, false},
		{"26.9", false, false},
		{"27.0", true, false},
		{"27.1.2", true, false},
		{"28.0", true, false},
		{"", false, true},
		{"unknown", false, true},
		{"0.0", false, true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			got, err := needsDaemonDisclaim(tc.version)
			if got != tc.want || (err != nil) != tc.invalid {
				t.Fatalf("got (%t, %v)", got, err)
			}
		})
	}
}

func TestExecDisclaimedFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	if err := execDisclaimed(path, []string{path}, os.Environ()); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("execDisclaimed error = %v, want ENOENT", err)
	}
}

func TestDisclaimedDaemonLifecycle(t *testing.T) {
	// Re-exec this test binary as the actual daemon, exercising the go-daemon
	// initialization pipe, inherited PID lock, readiness marker, and restart.
	if dir := os.Getenv("_LLM_SEARCH_TEST_DAEMON_DIR"); dir != "" {
		args := []string{
			os.Getenv("_LLM_SEARCH_TEST_DAEMON_OPERATION"),
			"--data-dir", dir, "--codex-home", filepath.Join(dir, "codex"),
			"--claude-home", filepath.Join(dir, "claude"), "--listen", "127.0.0.1:0",
		}
		if err := Run(args, "test", os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	version, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := needsDaemonDisclaim(version)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Skip("responsibility test requires macOS 27 or later")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "codex", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := func(operation string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestDisclaimedDaemonLifecycle$")
		cmd.Env = append(os.Environ(), "_LLM_SEARCH_TEST_DAEMON_DIR="+dir, "_LLM_SEARCH_TEST_DAEMON_OPERATION="+operation)
		output, err := cmd.CombinedOutput()
		if err != nil {
			log, _ := os.ReadFile(filepath.Join(dir, "app.log"))
			return fmt.Errorf("%s: %w\n%s\n%s", operation, err, output, log)
		}
		return nil
	}
	t.Cleanup(func() {
		if err := run("--daemon-stop"); err != nil {
			t.Error(err)
		}
	})
	state := newDaemonState(dir)
	check := func() int {
		t.Helper()
		process, running, err := state.findProcess()
		if err != nil || !running {
			t.Fatalf("findProcess: %v, running %t", err, running)
		}
		pid := process.Pid
		responsible, err := testResponsiblePID(pid)
		if err != nil {
			t.Fatal(err)
		}
		if responsible != pid {
			t.Fatalf("PID %d has responsible PID %d", pid, responsible)
		}
		if err := run("--daemon-status"); err != nil {
			t.Fatal(err)
		}
		return pid
	}
	if err := run("--daemon"); err != nil {
		t.Fatal(err)
	}
	first := check()
	if err := run("--daemon"); err != nil {
		t.Fatal(err)
	}
	if pid := check(); pid != first {
		t.Fatalf("duplicate start changed PID: %d -> %d", first, pid)
	}
	if err := run("--daemon-restart"); err != nil {
		t.Fatal(err)
	}
	if pid := check(); pid == first {
		t.Fatal("restart did not replace daemon")
	}
	if err := run("--daemon-stop"); err != nil {
		t.Fatal(err)
	}
	if _, running, err := state.findProcess(); err != nil || running {
		t.Fatalf("daemon still running: %t, %v", running, err)
	}
}

// Query the same Quarantine operation used by
// responsibility_get_pid_responsible_for_pid, without requiring cgo or root.
func testResponsiblePID(pid int) (int, error) {
	var responsible, unique uint64
	policy, err := syscall.BytePtrFromString("Quarantine")
	if err != nil {
		return 0, err
	}
	args := &struct {
		pid                 uint64
		responsible, unique unsafe.Pointer
		reserved            [2]uint64
	}{pid: uint64(pid), responsible: unsafe.Pointer(&responsible), unique: unsafe.Pointer(&unique)}
	var pins runtime.Pinner
	defer pins.Unpin()
	pins.Pin(&responsible)
	pins.Pin(&unique)
	pins.Pin(args)
	_, _, errno := syscall.Syscall(381, uintptr(unsafe.Pointer(policy)), 180, uintptr(unsafe.Pointer(args)))
	if errno != 0 {
		return 0, errno
	}
	return int(responsible), nil
}
