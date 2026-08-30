package search

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type logWriter chan string

func (writer logWriter) Write(data []byte) (int, error) {
	writer <- string(data)
	return len(data), nil
}

func TestRunRejectsRemovedSubcommands(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"index", "serve"} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			err := Run([]string{command}, "dev", &bytes.Buffer{}, &stderr)
			if err == nil || !strings.Contains(err.Error(), "subcommands are not supported") {
				t.Fatalf("run(%q) error = %v, stderr = %q", command, err, stderr.String())
			}
		})
	}
}

func TestRunVersionDoesNotStartDaemon(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	if err := Run([]string{"--daemon", "--version"}, "test-version", &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "test-version\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunRejectsDBOption(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	err := Run([]string{"--db", filepath.Join(t.TempDir(), "index.db")}, "dev", &bytes.Buffer{}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -db") {
		t.Fatalf("run() error = %v, stderr = %q", err, stderr.String())
	}
}

func TestRunRejectsMultipleDaemonOperations(t *testing.T) {
	t.Parallel()
	err := Run([]string{"--daemon", "--daemon-stop"}, "dev", &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestAbsolutePath(t *testing.T) {
	t.Parallel()

	if got, err := absolutePath(""); err != nil || got != "" {
		t.Fatalf("absolutePath(empty) = (%q, %v)", got, err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(workingDirectory, "relative", "path")
	got, err := absolutePath(filepath.Join("relative", "path"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("absolutePath(relative) = %q, want %q", got, want)
	}
}

func TestRunReportsAbsoluteSessionHome(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	err := Run([]string{
		"--codex-home", "missing-relative-codex-home",
		"--claude-home=",
		"--data-dir", filepath.Join(t.TempDir(), "data"),
		"--index-interval", "0",
	}, "dev", &stdout, &bytes.Buffer{})
	if err == nil {
		t.Fatal("run() succeeded with a missing Codex home")
	}
	absoluteHome, err := filepath.Abs("missing-relative-codex-home")
	if err != nil {
		t.Fatal(err)
	}
	want := "Indexing Codex sessions from " + absoluteHome + "...\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunReportsInitialIndexStart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	codexHome := filepath.Join(root, "missing-codex-home")
	dataDir := filepath.Join(root, "data")
	var stdout bytes.Buffer

	err := Run([]string{
		"--codex-home", codexHome,
		"--claude-home=",
		"--data-dir", dataDir,
		"--index-interval", "0",
	}, "dev", &stdout, &bytes.Buffer{})
	if err == nil {
		t.Fatal("run() succeeded with a missing Codex home")
	}
	want := "Indexing Codex sessions from " + codexHome + "...\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestOpenIndexedStoreCreatesDatabaseAndIndex(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	sessionDir := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(sessionDir, "rollout-2026-08-29T09-00-00-"+testSessionID+".jsonl")
	if err := os.WriteFile(sessionPath, []byte(`{"timestamp":"2026-08-29T00:00:00Z","role":"user","message":"created on startup"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "new-data", "index.db")

	store, stats, err := openIndexedStore(context.Background(), SessionHomes{Codex: codexHome}, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if stats.Sessions != 1 || stats.Changed != 1 || stats.Records != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database was not created: %v", err)
	}
	hits, err := store.Search(context.Background(), "created on startup", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != testSessionID {
		t.Fatalf("startup index search hits = %+v", hits)
	}
}

func TestRunPeriodicIndexerLogsFailuresAndKeepsRunning(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	logLines := make(logWriter, 4)
	calls := make(chan int, 2)
	logger := log.New(logLines, "", 0)
	callCount := 0

	go func() {
		defer close(done)
		runPeriodicIndexer(ctx, time.Millisecond, logger, func(context.Context) (IndexStats, error) {
			callCount++
			calls <- callCount
			if callCount == 1 {
				return IndexStats{}, errors.New("temporary failure")
			}
			return IndexStats{Sessions: 2, Changed: 1, Unchanged: 1, Records: 3}, nil
		})
	}()

	for range 2 {
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("periodic indexer did not retry")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("periodic indexer did not stop")
	}
	close(logLines)

	var logs strings.Builder
	for line := range logLines {
		logs.WriteString(line)
	}
	output := logs.String()
	if !strings.Contains(output, "Background index failed: temporary failure") ||
		strings.Contains(output, "Background index completed:") {
		t.Fatalf("unexpected logs: %s", output)
	}
}

func TestRunPeriodicIndexerHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	runPeriodicIndexer(ctx, time.Hour, log.New(logWriter(make(chan string, 1)), "", 0), func(context.Context) (IndexStats, error) {
		called = true
		return IndexStats{}, nil
	})
	if called {
		t.Fatal("index ran after cancellation")
	}
}
