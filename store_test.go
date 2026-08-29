package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestOpenStoreMigratesSchema(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE app_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		INSERT INTO app_meta(key, value) VALUES ('index_version', '7');
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			path TEXT NOT NULL,
			archived INTEGER NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			cwd TEXT NOT NULL DEFAULT '',
			started_at_ms INTEGER,
			updated_at_ms INTEGER,
			size INTEGER NOT NULL,
			mtime_ns INTEGER NOT NULL,
			scan_generation TEXT NOT NULL
		);
		CREATE TABLE records (
			id INTEGER PRIMARY KEY,
			session_id TEXT NOT NULL,
			line_number INTEGER NOT NULL,
			byte_offset INTEGER NOT NULL,
			timestamp_ms INTEGER,
			role TEXT NOT NULL DEFAULT '',
			phase TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT '',
			text TEXT NOT NULL
		);
		INSERT INTO records(
			session_id, line_number, byte_offset, role, phase, kind, text
		) VALUES ('old-session', 1, 0, 'user', '', 'user', 'obsolete row')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var columns int
	if err := store.db.QueryRow(`
		SELECT count(*) FROM pragma_table_info('sessions') WHERE name = 'line_count'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 1 {
		t.Fatalf("line_count columns = %d, want 1", columns)
	}
	if err := store.db.QueryRow(`
		SELECT count(*) FROM pragma_table_info('records')
		WHERE name IN ('byte_offset', 'kind')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatalf("obsolete record columns = %d, want 0", columns)
	}
	version, err := store.indexVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "" {
		t.Fatalf("index version = %q, want empty after record table rebuild", version)
	}
}

func TestSearchHistory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 55 {
		if err := store.RecordSearch(context.Background(), fmt.Sprintf("query-%02d", index), 50); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordSearch(context.Background(), "query-20", 50); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	history, err := store.ListSearchHistory(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 50 {
		t.Fatalf("history length = %d, want 50", len(history))
	}
	if history[0] != "query-20" {
		t.Fatalf("most recent history = %q, want query-20", history[0])
	}
	for _, query := range history {
		if query == "query-00" {
			t.Fatalf("oldest query was not removed: %v", history)
		}
	}
}
