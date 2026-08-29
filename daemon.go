package main

import (
	"fmt"
	"os"
	"path/filepath"

	daemon "github.com/sevlyar/go-daemon"
)

type daemonState struct {
	context *daemon.Context
	child   *os.Process
	pidPath string
	logPath string
}

func startDaemon(dbPath string, enforcePermissions bool) (daemonState, error) {
	directory, err := filepath.Abs(filepath.Dir(dbPath))
	if err != nil {
		return daemonState{}, fmt.Errorf("resolve daemon directory: %w", err)
	}
	if err := ensurePrivateDir(directory, enforcePermissions); err != nil {
		return daemonState{}, err
	}

	pidPath := filepath.Join(directory, "app.pid")
	logPath := filepath.Join(directory, "app.log")
	context := &daemon.Context{
		PidFileName: pidPath,
		PidFilePerm: 0o600,
		LogFileName: logPath,
		LogFilePerm: 0o600,
		Umask:       0o077,
	}
	child, err := context.Reborn()
	if err != nil {
		return daemonState{}, fmt.Errorf("start daemon: %w", err)
	}
	return daemonState{
		context: context,
		child:   child,
		pidPath: pidPath,
		logPath: logPath,
	}, nil
}

func (state daemonState) release() {
	if state.context != nil {
		_ = state.context.Release()
	}
}
