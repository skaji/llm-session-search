package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestOpenStoreCreatesSourceSchema(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var columns int
	if err := store.db.QueryRow(`
		SELECT count(*) FROM pragma_table_info('sessions')
		WHERE name IN ('key', 'source', 'source_id', 'line_count')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 4 {
		t.Fatalf("source-aware session columns = %d, want 4", columns)
	}
	if err := store.db.QueryRow(`
		SELECT count(*) FROM pragma_table_info('records') WHERE name = 'session_key'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 1 {
		t.Fatalf("session_key columns = %d, want 1", columns)
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
