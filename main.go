package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

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
	dbPath := flags.String("db", defaults.dbPath, "SQLite index path")
	listen := flags.String("listen", "127.0.0.1:8787", "HTTP listen address")
	indexInterval := flags.Duration("index-interval", time.Minute, "Background index interval (0 disables it)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q; subcommands are not supported", flags.Arg(0))
	}
	if *indexInterval < 0 {
		return errors.New("index interval must not be negative")
	}

	enforcePermissions := filepath.Clean(*dbPath) == filepath.Clean(defaults.dbPath)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	started := time.Now()
	store, stats, err := openIndexedStore(ctx, *codexHome, *dbPath, enforcePermissions)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	_, _ = fmt.Fprintf(stdout, "Indexed %d sessions (%d changed, %d unchanged, %d records) in %s\n",
		stats.Sessions, stats.Changed, stats.Unchanged, stats.Records, time.Since(started).Round(time.Millisecond))
	_, _ = fmt.Fprintf(stdout, "Database: %s\n", *dbPath)

	server := &http.Server{
		Addr:              *listen,
		Handler:           NewWebHandler(store),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger := log.New(stderr, "", log.LstdFlags)
	var indexerDone <-chan struct{}
	if *indexInterval > 0 {
		done := make(chan struct{})
		indexerDone = done
		go func() {
			defer close(done)
			runPeriodicIndexer(ctx, *indexInterval, logger, func(indexCtx context.Context) (IndexStats, error) {
				return IndexSessions(indexCtx, store, *codexHome)
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
		_, _ = fmt.Fprintf(stdout, "Background indexing %s every %s\n", *codexHome, *indexInterval)
	}
	err = server.ListenAndServe()
	stop()
	if indexerDone != nil {
		<-indexerDone
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func openIndexedStore(ctx context.Context, codexHome, dbPath string, enforcePermissions bool) (*Store, IndexStats, error) {
	if err := ensurePrivateDir(filepath.Dir(dbPath), enforcePermissions); err != nil {
		return nil, IndexStats{}, err
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		return nil, IndexStats{}, err
	}
	stats, err := IndexSessions(ctx, store, codexHome)
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

		started := time.Now()
		stats, err := index(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logger.Printf("Background index failed: %v", err)
		} else {
			logger.Printf("Background index completed: %d sessions (%d changed, %d unchanged, %d records) in %s",
				stats.Sessions, stats.Changed, stats.Unchanged, stats.Records, time.Since(started).Round(time.Millisecond))
		}
		timer.Reset(interval)
	}
}

type paths struct {
	codexHome string
	dbPath    string
}

func defaultPaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, fmt.Errorf("find home directory: %w", err)
	}
	return paths{
		codexHome: filepath.Join(home, ".codex"),
		dbPath:    filepath.Join(home, ".llm-session-search", "index.db"),
	}, nil
}

func ensurePrivateDir(path string, enforcePermissions bool) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	if enforcePermissions {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("set data directory permissions: %w", err)
		}
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

Index Codex JSONL session files and start the local search web application.

Options:`)
	flags.PrintDefaults()
}
