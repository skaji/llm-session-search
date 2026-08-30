package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var uuidPattern = regexp.MustCompile(`(?i)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

const (
	maxIndexedStringBytes = 2 << 20
	sourceCodex           = "codex"
	sourceClaude          = "claude"
)

type SessionHomes struct {
	Codex  string
	Claude string
}

type sessionSource struct {
	id   string
	name string
	home string
}

func (homes SessionHomes) sources() []sessionSource {
	var sources []sessionSource
	if homes.Codex != "" {
		sources = append(sources, sessionSource{id: sourceCodex, name: "Codex", home: homes.Codex})
	}
	if homes.Claude != "" {
		sources = append(sources, sessionSource{id: sourceClaude, name: "Claude", home: homes.Claude})
	}
	return sources
}

func (source sessionSource) discover() ([]sessionFile, bool, error) {
	switch source.id {
	case sourceCodex:
		return discoverCodexSessionFiles(source.home)
	case sourceClaude:
		return discoverClaudeSessionFiles(source.home)
	default:
		return nil, false, fmt.Errorf("unsupported session source %q", source.id)
	}
}

type IndexStats struct {
	Sessions  int
	Changed   int
	Unchanged int
	Records   int
}

type sessionFile struct {
	source     string
	path       string
	archived   bool
	id         string
	title      string
	titleKnown bool
	info       fs.FileInfo
}

type extractedLine struct {
	text       string
	role       string
	phase      string
	cwd        string
	timestamps []time.Time
	title      string
	forceTitle bool
}

type sessionScan struct {
	offset     int64
	lineNumber int
	records    int
	cwd        string
	startedAt  time.Time
	updatedAt  time.Time
	title      string
}

func IndexSessions(ctx context.Context, store *Store, homes SessionHomes) (IndexStats, error) {
	files, err := discoverSessionFiles(homes)
	if err != nil {
		return IndexStats{}, err
	}
	generation := strconv.FormatInt(time.Now().UnixNano(), 10)
	stats := IndexStats{Sessions: len(files)}

	for _, file := range files {
		state, err := store.sessionIndexState(ctx, file.source, file.id)
		if err != nil {
			return stats, err
		}
		current := state.found && state.path == file.path && state.size == file.info.Size() && state.mtimeNS == file.info.ModTime().UnixNano()
		if current {
			if err := store.markSessionSeen(ctx, state.key, file.path, file.archived, file.title, file.titleKnown, generation); err != nil {
				return stats, err
			}
			stats.Unchanged++
			continue
		}

		var records int
		if state.found && state.path == file.path && file.info.Size() > state.size {
			records, err = appendSessionFile(ctx, store, file, generation, state)
		} else {
			records, err = indexSessionFile(ctx, store, file, generation)
		}
		if err != nil {
			return stats, fmt.Errorf("index %s: %w", file.path, err)
		}
		stats.Changed++
		stats.Records += records
	}
	if err := store.removeStaleSessions(ctx, generation); err != nil {
		return stats, fmt.Errorf("remove stale sessions: %w", err)
	}
	return stats, nil
}

func discoverSessionFiles(homes SessionHomes) ([]sessionFile, error) {
	var files []sessionFile
	foundSource := false
	for _, source := range homes.sources() {
		discovered, found, err := source.discover()
		if err != nil {
			return nil, err
		}
		files = append(files, discovered...)
		foundSource = foundSource || found
	}
	if !foundSource {
		return nil, errors.New("no Codex or Claude session directories found")
	}
	slices.SortFunc(files, func(a, b sessionFile) int {
		if bySource := strings.Compare(a.source, b.source); bySource != 0 {
			return bySource
		}
		return strings.Compare(a.path, b.path)
	})
	return files, nil
}

func sessionIDFromPath(path string) string {
	if match := uuidPattern.FindStringSubmatch(filepath.Base(path)); len(match) > 1 {
		return strings.ToLower(match[1])
	}
	sum := sha256.Sum256([]byte(path))
	return "file-" + hex.EncodeToString(sum[:12])
}

func indexSessionFile(ctx context.Context, store *Store, file sessionFile, generation string) (int, error) {
	handle, err := os.Open(file.path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = handle.Close() }()

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var sessionKey int64
	if err := tx.QueryRowContext(ctx, `
        INSERT INTO sessions(
            source, source_id, path, archived, title, cwd, started_at_ms, updated_at_ms,
            size, mtime_ns, scan_generation
        ) VALUES (?, ?, ?, ?, ?, '', NULL, NULL, 0, 0, ?)
        ON CONFLICT(source, source_id) DO UPDATE SET
            path = excluded.path,
            archived = excluded.archived,
            title = excluded.title,
			scan_generation = excluded.scan_generation
		RETURNING key`,
		file.source, file.id, file.path, file.archived, file.title, generation).Scan(&sessionKey); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE session_key = ?`, sessionKey); err != nil {
		return 0, err
	}
	insert, err := tx.PrepareContext(ctx, `
        INSERT INTO records(
			session_key, line_number, timestamp_ms, role, phase, text
		) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = insert.Close() }()

	scan := sessionScan{title: file.title}
	if timestamp, ok := timestampFromFilename(file.path); ok {
		scan.startedAt = timestamp
	}
	scan, err = scanSessionLines(ctx, handle, insert, sessionKey, file.source, scan)
	if err != nil {
		return 0, err
	}

	if scan.updatedAt.IsZero() {
		scan.updatedAt = file.info.ModTime()
	}
	afterInfo, err := handle.Stat()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
        UPDATE sessions
		SET title = ?, cwd = ?, started_at_ms = ?, updated_at_ms = ?, size = ?, mtime_ns = ?,
		    line_count = ?
		WHERE key = ?`, scan.title, scan.cwd, nullableUnixMilli(scan.startedAt), nullableUnixMilli(scan.updatedAt),
		scan.offset, afterInfo.ModTime().UnixNano(),
		scan.lineNumber, sessionKey); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return scan.records, nil
}

func appendSessionFile(
	ctx context.Context,
	store *Store,
	file sessionFile,
	generation string,
	state sessionIndexState,
) (int, error) {
	handle, err := os.Open(file.path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = handle.Close() }()
	if _, err := handle.Seek(state.size, io.SeekStart); err != nil {
		return 0, err
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	insert, err := tx.PrepareContext(ctx, `
        INSERT INTO records(
			session_key, line_number, timestamp_ms, role, phase, text
		) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = insert.Close() }()

	scan := sessionScan{offset: state.size, lineNumber: state.lastLine, cwd: state.cwd, title: state.title}
	if state.startedAtMS.Valid {
		scan.startedAt = time.UnixMilli(state.startedAtMS.Int64)
	}
	if state.updatedAtMS.Valid {
		scan.updatedAt = time.UnixMilli(state.updatedAtMS.Int64)
	}
	scan, err = scanSessionLines(ctx, handle, insert, state.key, file.source, scan)
	if err != nil {
		return 0, err
	}

	afterInfo, err := handle.Stat()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
        UPDATE sessions
		SET path = ?, archived = ?, title = ?, cwd = ?,
		    started_at_ms = ?, updated_at_ms = ?, size = ?, mtime_ns = ?, line_count = ?,
            scan_generation = ?
		WHERE key = ?`, file.path, file.archived, scan.title, scan.cwd,
		nullableUnixMilli(scan.startedAt), nullableUnixMilli(scan.updatedAt), scan.offset,
		afterInfo.ModTime().UnixNano(), scan.lineNumber, generation, state.key); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return scan.records, nil
}

func scanSessionLines(
	ctx context.Context,
	reader io.Reader,
	insert *sql.Stmt,
	sessionKey int64,
	source string,
	scan sessionScan,
) (sessionScan, error) {
	buffer := bufio.NewReaderSize(reader, 256*1024)
	for {
		lineOffset := scan.offset
		line, readErr := buffer.ReadBytes('\n')
		scan.offset += int64(len(line))
		trimmed := bytes.TrimSpace(line)
		if errors.Is(readErr, io.EOF) && len(trimmed) > 0 && !json.Valid(trimmed) {
			scan.offset = lineOffset
			break
		}
		if len(line) > 0 {
			scan.lineNumber++
			extracted := extractSessionLine(source, line)
			if scan.cwd == "" && extracted.cwd != "" {
				scan.cwd = extracted.cwd
			}
			if extracted.title != "" && (extracted.forceTitle || scan.title == "") {
				scan.title = extracted.title
			}
			for _, timestamp := range extracted.timestamps {
				if scan.startedAt.IsZero() || timestamp.Before(scan.startedAt) {
					scan.startedAt = timestamp
				}
				if scan.updatedAt.IsZero() || timestamp.After(scan.updatedAt) {
					scan.updatedAt = timestamp
				}
			}
			if extracted.text != "" && (extracted.role == "user" || extracted.role == "assistant") {
				var timestamp any
				if len(extracted.timestamps) > 0 {
					timestamp = extracted.timestamps[0].UnixMilli()
				}
				if _, err := insert.ExecContext(ctx, sessionKey, scan.lineNumber, timestamp,
					extracted.role, extracted.phase, extracted.text); err != nil {
					return scan, err
				}
				scan.records++
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return scan, readErr
		}
	}
	return scan, nil
}

func nullableUnixMilli(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UnixMilli()
}

func extractSessionLine(source string, line []byte) extractedLine {
	switch source {
	case sourceCodex:
		return extractCodexJSONLine(line)
	case sourceClaude:
		return extractClaudeJSONLine(line)
	default:
		return extractedLine{}
	}
}

func decodeJSONLine(line []byte) (any, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, false
	}

	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

func isBase64DataURL(value string) bool {
	return strings.HasPrefix(value, "data:") && strings.Contains(value[:min(len(value), 128)], ";base64,")
}

func extractMetadata(key string, value any, depth int, result *extractedLine) {
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for childKey := range value {
			keys = append(keys, childKey)
		}
		slices.Sort(keys)
		for _, childKey := range keys {
			extractMetadata(childKey, value[childKey], depth+1, result)
		}
	case []any:
		for _, child := range value {
			extractMetadata(key, child, depth+1, result)
		}
	case string:
		if depth <= 2 && isTimestampKey(key) {
			if timestamp, ok := parseTimestamp(value); ok {
				result.timestamps = append(result.timestamps, timestamp)
			}
		}
		switch {
		case depth <= 2 && strings.EqualFold(key, "phase"):
			if result.phase == "" {
				result.phase = value
			}
		case depth <= 2 && strings.EqualFold(key, "cwd"):
			if result.cwd == "" {
				result.cwd = value
			}
		}
	case json.Number:
		if depth <= 2 && isTimestampKey(key) {
			if timestamp, ok := parseNumericTimestamp(value.String()); ok {
				result.timestamps = append(result.timestamps, timestamp)
			}
		}
	}
}

func limitString(value string) string {
	if len(value) <= maxIndexedStringBytes {
		return value
	}
	half := maxIndexedStringBytes / 2
	prefixEnd := half
	for prefixEnd > 0 && !utf8.RuneStart(value[prefixEnd]) {
		prefixEnd--
	}
	suffixStart := len(value) - half
	for suffixStart < len(value) && !utf8.RuneStart(value[suffixStart]) {
		suffixStart++
	}
	return value[:prefixEnd] + "\n…[truncated]…\n" + value[suffixStart:]
}

func isTimestampKey(key string) bool {
	switch strings.ToLower(key) {
	case "timestamp", "started_at", "updated_at", "created_at", "completed_at", "create_time":
		return true
	default:
		return false
	}
}

func parseTimestamp(value string) (time.Time, bool) {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return validTimestamp(parsed)
		}
	}
	return parseNumericTimestamp(value)
}

func parseNumericTimestamp(value string) (time.Time, bool) {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number <= 0 {
		return time.Time{}, false
	}
	var seconds float64
	switch {
	case number > 1e17:
		seconds = number / 1e9
	case number > 1e14:
		seconds = number / 1e6
	case number > 1e11:
		seconds = number / 1e3
	default:
		seconds = number
	}
	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * 1e9)
	return validTimestamp(time.Unix(whole, nanos))
}

func validTimestamp(value time.Time) (time.Time, bool) {
	year := value.Year()
	return value, year >= 2000 && year <= 2100
}
