package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS search_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    query TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS sessions (
    key INTEGER PRIMARY KEY,
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    path TEXT NOT NULL,
    archived INTEGER NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    cwd TEXT NOT NULL DEFAULT '',
    started_at_ms INTEGER,
    updated_at_ms INTEGER,
    size INTEGER NOT NULL,
    mtime_ns INTEGER NOT NULL,
    line_count INTEGER NOT NULL DEFAULT 0,
    scan_generation TEXT NOT NULL,
    UNIQUE(source, source_id)
);

CREATE INDEX IF NOT EXISTS sessions_updated_at_idx
    ON sessions(updated_at_ms DESC);

CREATE TABLE IF NOT EXISTS records (
	id INTEGER PRIMARY KEY,
	session_key INTEGER NOT NULL REFERENCES sessions(key) ON DELETE CASCADE,
	line_number INTEGER NOT NULL,
	timestamp_ms INTEGER,
	role TEXT NOT NULL DEFAULT '',
	phase TEXT NOT NULL DEFAULT '',
	text TEXT NOT NULL,
    UNIQUE(session_key, line_number)
);

CREATE INDEX IF NOT EXISTS records_session_idx
    ON records(session_key, line_number);

CREATE VIRTUAL TABLE IF NOT EXISTS records_fts USING fts5(
    text,
    content='records',
    content_rowid='id',
    tokenize='trigram'
);

CREATE TRIGGER IF NOT EXISTS records_ai AFTER INSERT ON records BEGIN
    INSERT INTO records_fts(rowid, text) VALUES (new.id, new.text);
END;

CREATE TRIGGER IF NOT EXISTS records_ad AFTER DELETE ON records BEGIN
    INSERT INTO records_fts(records_fts, rowid, text)
        VALUES ('delete', old.id, old.text);
END;

CREATE TRIGGER IF NOT EXISTS records_au AFTER UPDATE ON records BEGIN
    INSERT INTO records_fts(records_fts, rowid, text)
        VALUES ('delete', old.id, old.text);
    INSERT INTO records_fts(rowid, text) VALUES (new.id, new.text);
END;
`

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set database permissions: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

type Session struct {
	Key         int64
	Source      string
	ID          string
	Path        string
	Archived    bool
	Title       string
	CWD         string
	StartedAtMS sql.NullInt64
	UpdatedAtMS sql.NullInt64
	Size        int64
}

type Record struct {
	LineNumber  int
	TimestampMS sql.NullInt64
	Role        string
	Phase       string
	Text        string
}

type SearchHit struct {
	Session
	Record
	Snippet    string
	MatchCount int
}

type sessionIndexState struct {
	key         int64
	path        string
	size        int64
	mtimeNS     int64
	cwd         string
	startedAtMS sql.NullInt64
	updatedAtMS sql.NullInt64
	lastLine    int
	title       string
	found       bool
}

func (s *Store) sessionIndexState(ctx context.Context, source, id string) (sessionIndexState, error) {
	var state sessionIndexState
	err := s.db.QueryRowContext(ctx, `
		SELECT s.key, s.path, s.size, s.mtime_ns, s.cwd, s.started_at_ms, s.updated_at_ms,
		       s.line_count, s.title
		FROM sessions s
		WHERE s.source = ? AND s.source_id = ?`, source, id).Scan(
		&state.key, &state.path, &state.size, &state.mtimeNS, &state.cwd,
		&state.startedAtMS, &state.updatedAtMS, &state.lastLine, &state.title,
	)
	if err == sql.ErrNoRows {
		return sessionIndexState{}, nil
	}
	if err != nil {
		return sessionIndexState{}, err
	}
	state.found = true
	return state, nil
}

func (s *Store) markSessionSeen(ctx context.Context, key int64, path string, archived bool, title string, titleKnown bool, generation string) error {
	statement := `UPDATE sessions SET path = ?, archived = ?, scan_generation = ? WHERE key = ?`
	args := []any{path, archived, generation, key}
	if titleKnown {
		statement = `UPDATE sessions SET path = ?, archived = ?, title = ?, scan_generation = ? WHERE key = ?`
		args = []any{path, archived, title, generation, key}
	}
	_, err := s.db.ExecContext(ctx, statement, args...)
	return err
}

func (s *Store) removeStaleSessions(ctx context.Context, generation string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE scan_generation <> ?`, generation)
	return err
}

func (s *Store) RecordSearch(ctx context.Context, query string, limit int) error {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	if limit <= 0 {
		limit = 50
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_history WHERE query = ?`, query); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO search_history(query) VALUES (?)`, query); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM search_history
		WHERE id NOT IN (
			SELECT id FROM search_history ORDER BY id DESC LIMIT ?
		)`, limit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListSearchHistory(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT query FROM search_history ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var history []string
	for rows.Next() {
		var query string
		if err := rows.Scan(&query); err != nil {
			return nil, err
		}
		history = append(history, query)
	}
	return history, rows.Err()
}

func (s *Store) Search(ctx context.Context, query string, limit, offset int) ([]SearchHit, error) {
	terms := parseSearchQuery(query)
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	termSQL, args := termHitsSQL(terms, 0)
	statement := `
		WITH term_hits(term_number, session_key, line_number) AS (` + termSQL + `),
		matches AS (
			SELECT session_key, max(line_number) AS line_number,
			       count(DISTINCT line_number) AS match_count
			FROM term_hits
			GROUP BY session_key
			HAVING count(DISTINCT term_number) = ?
		)
		SELECT s.key, s.source, s.source_id, s.path, s.archived, s.title, s.cwd,
		       s.started_at_ms, s.updated_at_ms, s.size,
		       r.line_number, r.timestamp_ms, r.role, r.phase, r.text,
		       matches.match_count
		FROM matches
		JOIN records r
		  ON r.session_key = matches.session_key
		 AND r.line_number = matches.line_number
		JOIN sessions s ON s.key = r.session_key
        ORDER BY coalesce(s.updated_at_ms, 0) DESC,
                 coalesce(r.timestamp_ms, 0) DESC
        LIMIT ? OFFSET ?`
	args = append(args, len(terms), limit, offset)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []SearchHit
	for rows.Next() {
		var hit SearchHit
		if err := scanHit(rows, &hit); err != nil {
			return nil, err
		}
		hit.Snippet = makeSnippet(hit.Text, terms, 180)
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

func scanHit(rows *sql.Rows, hit *SearchHit) error {
	var archived int
	err := rows.Scan(
		&hit.Key, &hit.Source, &hit.ID, &hit.Path, &archived, &hit.Title, &hit.CWD,
		&hit.StartedAtMS, &hit.UpdatedAtMS, &hit.Size,
		&hit.LineNumber, &hit.TimestampMS, &hit.Role, &hit.Phase, &hit.Text,
		&hit.MatchCount,
	)
	hit.Archived = archived != 0
	return err
}

func (s *Store) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key, source, source_id, path, archived, title, cwd, started_at_ms, updated_at_ms, size
		FROM sessions
        ORDER BY coalesce(updated_at_ms, 0) DESC
        LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var sessions []Session
	for rows.Next() {
		var session Session
		var archived int
		if err := rows.Scan(&session.Key, &session.Source, &session.ID, &session.Path, &archived, &session.Title, &session.CWD,
			&session.StartedAtMS, &session.UpdatedAtMS, &session.Size); err != nil {
			return nil, err
		}
		session.Archived = archived != 0
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) GetSession(ctx context.Context, source, id string) (Session, error) {
	var session Session
	var archived int
	err := s.db.QueryRowContext(ctx, `
		SELECT key, source, source_id, path, archived, title, cwd, started_at_ms, updated_at_ms, size
		FROM sessions WHERE source = ? AND source_id = ?`, source, id).Scan(
		&session.Key, &session.Source, &session.ID, &session.Path, &archived, &session.Title, &session.CWD,
		&session.StartedAtMS, &session.UpdatedAtMS, &session.Size,
	)
	session.Archived = archived != 0
	return session, err
}

func (s *Store) SessionRecords(ctx context.Context, sessionKey int64, query string, limit int) ([]Record, error) {
	terms := parseSearchQuery(query)
	var (
		rows *sql.Rows
		err  error
	)
	if len(terms) == 0 {
		rows, err = s.db.QueryContext(ctx, `
			SELECT line_number, timestamp_ms, role, phase, text
			FROM records
			WHERE session_key = ?
			ORDER BY line_number LIMIT ?`, sessionKey, limit)
	} else {
		termSQL, args := termHitsSQL(terms, sessionKey)
		statement := `
            WITH term_hits(term_number, line_number) AS (` + termSQL + `),
            matched_lines AS (
                SELECT DISTINCT line_number FROM term_hits
                WHERE (SELECT count(DISTINCT term_number) FROM term_hits) = ?
            )
			SELECT r.line_number, r.timestamp_ms, r.role, r.phase, r.text
			FROM matched_lines
			JOIN records r ON r.session_key = ? AND r.line_number = matched_lines.line_number
			ORDER BY r.line_number LIMIT ?`
		args = append(args, len(terms), sessionKey, limit)
		rows, err = s.db.QueryContext(ctx, statement, args...)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var records []Record
	for rows.Next() {
		var record Record
		if err := rows.Scan(&record.LineNumber, &record.TimestampMS, &record.Role, &record.Phase, &record.Text); err != nil {
			return nil, err
		}
		record.Text = makeSnippet(record.Text, terms, 600)
		records = append(records, record)
	}
	return records, rows.Err()
}

func termHitsSQL(terms []string, sessionKey int64) (string, []any) {
	parts := make([]string, 0, len(terms))
	args := make([]any, 0, len(terms)*3)
	columns := "r.session_key, r.line_number"
	scope := ""
	if sessionKey != 0 {
		columns = "r.line_number"
		scope = "r.session_key = ? AND "
	}
	for index, term := range terms {
		args = append(args, index)
		if sessionKey != 0 {
			args = append(args, sessionKey)
		}
		if utf8.RuneCountInString(term) >= 3 {
			parts = append(parts, fmt.Sprintf(`
				SELECT ? AS term_number, %s
				FROM records_fts
				JOIN records r ON r.id = records_fts.rowid
				WHERE %srecords_fts MATCH ?`, columns, scope))
			args = append(args, ftsPhrase(term))
		} else {
			parts = append(parts, fmt.Sprintf(`
				SELECT ? AS term_number, %s
				FROM records r
				WHERE %sinstr(lower(r.text), lower(?)) > 0`, columns, scope))
			args = append(args, term)
		}
	}
	return strings.Join(parts, " UNION ALL "), args
}

func ftsPhrase(text string) string {
	return `"` + strings.ReplaceAll(text, `"`, `""`) + `"`
}

func makeSnippet(text string, terms []string, maxRunes int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}

	index := max(0, firstMatchRune(text, terms))
	start := max(0, index-maxRunes/3)
	end := min(start+maxRunes, len(runes))
	start = max(0, end-maxRunes)
	prefix := ""
	suffix := ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(runes) {
		suffix = "…"
	}
	return prefix + string(runes[start:end]) + suffix
}

func unixMilliString(value sql.NullInt64) string {
	if !value.Valid {
		return ""
	}
	return time.UnixMilli(value.Int64).Local().Format("2006-01-02 15:04:05")
}
