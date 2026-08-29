package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "llm-session-search:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	defaults, err := defaultPaths()
	if err != nil {
		return err
	}

	flags := flag.NewFlagSet("llm-session-search", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { printUsage(stderr, flags) }
	codexHome := flags.String("codex-home", defaults.codexHome, "Codex data directory")
	claudeHome := flags.String("claude-home", defaults.claudeHome, "Claude Code data directory")
	dataDir := flags.String("data-dir", defaults.dataDir, "Application data directory")
	listen := flags.String("listen", "127.0.0.1:8787", "HTTP listen address")
	indexInterval := flags.Duration("index-interval", time.Minute, "Background index interval (0 disables it)")
	daemonFlag := flags.Bool("daemon", false, "run in the background")
	daemonStatusFlag := flags.Bool("daemon-status", false, "report whether the background process is running")
	daemonStopFlag := flags.Bool("daemon-stop", false, "stop the background process")
	daemonRestartFlag := flags.Bool("daemon-restart", false, "restart the background process")
	versionFlag := flags.Bool("version", false, "show version")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *versionFlag {
		_, _ = fmt.Fprintln(stdout, version)
		return nil
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q; subcommands are not supported", flags.Arg(0))
	}
	if *indexInterval < 0 {
		return errors.New("index interval must not be negative")
	}

	operation, err := selectDaemonOperation(*daemonFlag, *daemonStatusFlag, *daemonStopFlag, *daemonRestartFlag)
	if err != nil {
		return err
	}
	absoluteDataDir, err := filepath.Abs(*dataDir)
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}
	var daemonChild *daemonState
	if operation != daemonOperationNone {
		result, err := handleDaemonOperation(operation, absoluteDataDir, stdout)
		if err != nil {
			return err
		}
		if !result.runServer {
			return nil
		}
		daemonChild = &result.state
		defer result.state.release()
	}
	dbPath := filepath.Join(absoluteDataDir, "index.db")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	homes := SessionHomes{Codex: *codexHome, ClaudeCode: *claudeHome}
	for _, source := range homes.sources() {
		_, _ = fmt.Fprintf(stdout, "Indexing %s sessions from %s...\n", source.name, source.home)
	}
	started := time.Now()
	store, stats, err := openIndexedStore(ctx, homes, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	_, _ = fmt.Fprintf(stdout, "Indexed %d sessions (%d changed, %d unchanged, %d records) in %s\n",
		stats.Sessions, stats.Changed, stats.Unchanged, stats.Records, time.Since(started).Round(time.Millisecond))
	_, _ = fmt.Fprintf(stdout, "Database: %s\n", dbPath)

	server := &http.Server{
		Addr:              *listen,
		Handler:           NewWebHandler(store),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}
	defer func() { _ = listener.Close() }()
	if daemonChild != nil {
		if err := daemonChild.markReady(); err != nil {
			return err
		}
	}

	logger := log.New(stderr, "", log.LstdFlags)
	var indexerDone <-chan struct{}
	if *indexInterval > 0 {
		done := make(chan struct{})
		indexerDone = done
		go func() {
			defer close(done)
			runPeriodicIndexer(ctx, *indexInterval, logger, func(indexCtx context.Context) (IndexStats, error) {
				return IndexSessions(indexCtx, store, homes)
			})
		}()
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	_, _ = fmt.Fprintf(stdout, "Listening on http://%s\n", *listen)
	if *indexInterval > 0 {
		_, _ = fmt.Fprintf(stdout, "Background indexing %s every %s\n", formatSessionHomes(homes), *indexInterval)
	}
	err = server.Serve(listener)
	stop()
	if indexerDone != nil {
		<-indexerDone
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func openIndexedStore(ctx context.Context, homes SessionHomes, dbPath string) (*Store, IndexStats, error) {
	if err := ensurePrivateDir(filepath.Dir(dbPath)); err != nil {
		return nil, IndexStats{}, err
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		return nil, IndexStats{}, err
	}
	stats, err := IndexSessions(ctx, store, homes)
	if err != nil {
		_ = store.Close()
		return nil, IndexStats{}, err
	}
	return store, stats, nil
}

func runPeriodicIndexer(ctx context.Context, interval time.Duration, logger *log.Logger, index func(context.Context) (IndexStats, error)) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		_, err := index(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logger.Printf("Background index failed: %v", err)
		}
		timer.Reset(interval)
	}
}

type paths struct {
	codexHome  string
	claudeHome string
	dataDir    string
}

func defaultPaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, fmt.Errorf("find home directory: %w", err)
	}
	claudeHome := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".claude")
	}
	return paths{
		codexHome:  filepath.Join(home, ".codex"),
		claudeHome: claudeHome,
		dataDir:    filepath.Join(home, ".llm-session-search"),
	}, nil
}

func formatSessionHomes(homes SessionHomes) string {
	sources := homes.sources()
	parts := make([]string, 0, len(sources))
	for _, source := range sources {
		parts = append(parts, source.name+" ("+source.home+")")
	}
	return strings.Join(parts, ", ")
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("set data directory permissions: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect data directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("data directory is not a directory: %s", path)
	}
	return nil
}

func printUsage(w io.Writer, flags *flag.FlagSet) {
	_, _ = fmt.Fprintln(w, `Usage:
  llm-session-search [options]

Index Codex and Claude Code JSONL sessions and start the local search web application.

Options:`)
	flags.PrintDefaults()
}
