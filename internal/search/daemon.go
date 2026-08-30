package search

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	daemon "github.com/sevlyar/go-daemon"
)

const (
	daemonStartTimeout = 30 * time.Second
	daemonStopTimeout  = 10 * time.Second
)

var errDaemonNotRunning = errors.New("daemon is not running")

type daemonOperation uint8

const (
	daemonOperationNone daemonOperation = iota
	daemonOperationStart
	daemonOperationStatus
	daemonOperationStop
	daemonOperationRestart
)

type daemonState struct {
	context *daemon.Context
	child   *os.Process
	pidPath string
	logPath string
}

type daemonResult struct {
	runServer bool
	state     daemonState
}

func selectDaemonOperation(start, status, stop, restart bool) (daemonOperation, error) {
	operations := []struct {
		selected  bool
		operation daemonOperation
	}{
		{start, daemonOperationStart},
		{status, daemonOperationStatus},
		{stop, daemonOperationStop},
		{restart, daemonOperationRestart},
	}
	selected := daemonOperationNone
	count := 0
	for _, candidate := range operations {
		if candidate.selected {
			selected = candidate.operation
			count++
		}
	}
	if count > 1 {
		return daemonOperationNone, errors.New("daemon options are mutually exclusive")
	}
	return selected, nil
}

func handleDaemonOperation(operation daemonOperation, dataDir string, stdout io.Writer) (daemonResult, error) {
	state := newDaemonState(dataDir)

	// Reborn re-executes the binary with the original arguments. A child started
	// by --daemon-restart must proceed to the server instead of restarting again.
	if daemon.WasReborn() && (operation == daemonOperationStart || operation == daemonOperationRestart) {
		if err := state.reborn(); err != nil {
			return daemonResult{}, err
		}
		if err := os.Chdir("/"); err != nil {
			return daemonResult{}, fmt.Errorf("change daemon working directory: %w", err)
		}
		return daemonResult{runServer: true, state: state}, nil
	}

	switch operation {
	case daemonOperationStatus:
		process, running, err := state.findProcess()
		if err != nil {
			return daemonResult{}, err
		}
		if !running {
			return daemonResult{}, errDaemonNotRunning
		}
		_, _ = fmt.Fprintf(stdout, "llm-session-search is running (pid %d)\n", process.Pid)
		return daemonResult{}, nil
	case daemonOperationStop:
		process, stopped, err := state.stop()
		if err != nil {
			return daemonResult{}, err
		}
		if stopped {
			_, _ = fmt.Fprintf(stdout, "Stopped llm-session-search (pid %d)\n", process.Pid)
		} else {
			_, _ = fmt.Fprintln(stdout, "llm-session-search is not running")
		}
		return daemonResult{}, nil
	case daemonOperationRestart:
		process, stopped, err := state.stop()
		if err != nil {
			return daemonResult{}, err
		}
		if stopped {
			_, _ = fmt.Fprintf(stdout, "Stopped llm-session-search (pid %d)\n", process.Pid)
		}
		return state.start(stdout)
	case daemonOperationStart:
		return state.start(stdout)
	default:
		return daemonResult{}, fmt.Errorf("unknown daemon operation %d", operation)
	}
}

func newDaemonState(dataDir string) daemonState {
	pidPath := filepath.Join(dataDir, "app.pid")
	logPath := filepath.Join(dataDir, "app.log")
	return daemonState{
		context: &daemon.Context{
			PidFileName: pidPath,
			PidFilePerm: 0o600,
			LogFileName: logPath,
			LogFilePerm: 0o600,
			Umask:       0o077,
		},
		pidPath: pidPath,
		logPath: logPath,
	}
}

func (state daemonState) start(stdout io.Writer) (daemonResult, error) {
	process, running, err := state.findProcess()
	if err != nil {
		return daemonResult{}, err
	}
	if running {
		_, _ = fmt.Fprintf(stdout, "llm-session-search is already running (pid %d)\n", process.Pid)
		return daemonResult{}, nil
	}
	if err := ensurePrivateDir(filepath.Dir(state.pidPath)); err != nil {
		return daemonResult{}, err
	}
	if err := state.reborn(); err != nil {
		if errors.Is(err, daemon.ErrWouldBlock) {
			process, running, findErr := state.waitForProcess(2 * time.Second)
			if findErr == nil && running {
				_, _ = fmt.Fprintf(stdout, "llm-session-search is already running (pid %d)\n", process.Pid)
				return daemonResult{}, nil
			}
		}
		return daemonResult{}, err
	}
	if state.child == nil {
		return daemonResult{runServer: true, state: state}, nil
	}
	if err := state.waitUntilReady(daemonStartTimeout); err != nil {
		_ = state.child.Signal(syscall.SIGTERM)
		return daemonResult{}, err
	}
	_, _ = fmt.Fprintf(stdout, "Started llm-session-search in the background (pid %d)\n", state.child.Pid)
	_, _ = fmt.Fprintf(stdout, "PID file: %s\n", state.pidPath)
	_, _ = fmt.Fprintf(stdout, "Log: %s\n", state.logPath)
	return daemonResult{}, nil
}

func (state *daemonState) reborn() error {
	child, err := state.context.Reborn()
	if err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	state.child = child
	return nil
}

func (state daemonState) stop() (*os.Process, bool, error) {
	process, running, err := state.findProcess()
	if err != nil || !running {
		return process, false, err
	}
	if process.Pid == os.Getpid() {
		return nil, false, errors.New("refusing to stop the current process")
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return nil, false, fmt.Errorf("stop daemon: %w", err)
	}
	deadline := time.Now().Add(daemonStopTimeout)
	for time.Now().Before(deadline) {
		_, running, err = state.findProcess()
		if err != nil {
			return nil, false, err
		}
		if !running {
			return process, true, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, false, fmt.Errorf("stop daemon: timed out after %s", daemonStopTimeout)
}

func (state daemonState) findProcess() (*os.Process, bool, error) {
	pid, err := daemon.ReadPidFile(state.pidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		locked, cleanupErr := removePIDFileIfUnlocked(state.pidPath)
		if cleanupErr != nil {
			return nil, false, fmt.Errorf("remove invalid PID file: %w", cleanupErr)
		}
		if locked {
			return nil, false, nil
		}
		return nil, false, nil
	}
	if pid <= 1 {
		if _, err := removePIDFileIfUnlocked(state.pidPath); err != nil {
			return nil, false, fmt.Errorf("remove invalid PID file: %w", err)
		}
		return nil, false, nil
	}
	locked, err := removePIDFileIfUnlocked(state.pidPath)
	if err != nil {
		return nil, false, fmt.Errorf("inspect PID file lock: %w", err)
	}
	if !locked {
		return nil, false, nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil, false, fmt.Errorf("find daemon process %d: %w", pid, err)
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		if errors.Is(err, syscall.EPERM) {
			return process, true, nil
		}
		return nil, false, nil
	}
	return process, true, nil
}

func (state daemonState) waitForProcess(timeout time.Duration) (*os.Process, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		process, running, err := state.findProcess()
		if err != nil || running || time.Now().After(deadline) {
			return process, running, err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (state daemonState) waitUntilReady(timeout time.Duration) error {
	defer func() { _ = state.removeReady(state.child.Pid) }()
	exited := make(chan error, 1)
	go func() {
		_, err := state.child.Wait()
		exited <- err
	}()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			if err != nil {
				return fmt.Errorf("daemon exited during startup: %w", err)
			}
			return errors.New("daemon exited during startup")
		default:
		}

		ready, err := daemonReady(state.readyPath(state.child.Pid), state.child.Pid)
		if err == nil && ready {
			if err := state.removeReady(state.child.Pid); err != nil {
				return err
			}
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become ready within %s", timeout)
}

func (state daemonState) markReady() error {
	path := state.readyPath(os.Getpid())
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return fmt.Errorf("mark daemon ready: %w", err)
	}
	return nil
}

func (state daemonState) readyPath(pid int) string {
	return filepath.Join(filepath.Dir(state.pidPath), fmt.Sprintf(".app.ready.%d", pid))
}

func (state daemonState) removeReady(pid int) error {
	if err := os.Remove(state.readyPath(pid)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove daemon readiness file: %w", err)
	}
	return nil
}

func daemonReady(path string, wantPID int) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 1 {
		return false, nil
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return false, nil
	}
	return pid == wantPID, nil
}

func removePIDFileIfUnlocked(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	lock := daemon.NewLockFile(file)
	if err := lock.Lock(); err != nil {
		_ = file.Close()
		if errors.Is(err, daemon.ErrWouldBlock) {
			return true, nil
		}
		return false, err
	}
	if err := lock.Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
}

func (state daemonState) release() {
	if state.context != nil {
		_ = state.context.Release()
	}
}
