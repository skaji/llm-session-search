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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	uuidPattern     = regexp.MustCompile(`(?i)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
	filenamePattern = regexp.MustCompile(`rollout-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2})-`)
)

const (
	maxIndexedStringBytes = 2 << 20
	currentIndexVersion   = "7"
)

type IndexStats struct {
	Sessions  int
	Changed   int
	Unchanged int
	Records   int
}

type sessionFile struct {
	path     string
	archived bool
	id       string
	info     fs.FileInfo
}

type extractedLine struct {
	text       string
	role       string
	phase      string
	cwd        string
	timestamps []time.Time
}

type sessionScan struct {
	offset     int64
	lineNumber int
	records    int
	cwd        string
	startedAt  time.Time
	updatedAt  time.Time
}

func IndexSessions(ctx context.Context, store *Store, codexHome string) (IndexStats, error) {
	files, err := discoverSessionFiles(codexHome)
	if err != nil {
		return IndexStats{}, err
	}
	titles := loadSessionTitles(filepath.Join(codexHome, "session_index.jsonl"))
	generation := strconv.FormatInt(time.Now().UnixNano(), 10)
	stats := IndexStats{Sessions: len(files)}
	version, err := store.indexVersion(ctx)
	if err != nil {
		return stats, err
	}
	forceReindex := version != currentIndexVersion

	for _, file := range files {
		state := sessionIndexState{}
		if !forceReindex {
			state, err = store.sessionIndexState(ctx, file.id)
			if err != nil {
				return stats, err
			}
		}
		current := state.found && state.path == file.path && state.size == file.info.Size() && state.mtimeNS == file.info.ModTime().UnixNano()
		if current {
			if err := store.markSessionSeen(ctx, file.id, file.path, file.archived, titles[file.id], generation); err != nil {
				return stats, err
			}
			stats.Unchanged++
			continue
		}

		var records int
		if state.found && state.path == file.path && file.info.Size() > state.size {
			records, err = appendSessionFile(ctx, store, file, titles[file.id], generation, state)
		} else {
			records, err = indexSessionFile(ctx, store, file, titles[file.id], generation)
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
	if forceReindex {
		if err := store.compactIndex(ctx); err != nil {
			return stats, fmt.Errorf("compact rebuilt index: %w", err)
		}
	}
	if err := store.setIndexVersion(ctx, currentIndexVersion); err != nil {
		return stats, fmt.Errorf("record index version: %w", err)
	}
	return stats, nil
}

func discoverSessionFiles(codexHome string) ([]sessionFile, error) {
	var files []sessionFile
	rootCount := 0
	for _, root := range []struct {
		name     string
		archived bool
	}{
		{name: "sessions"},
		{name: "archived_sessions", archived: true},
	} {
		dir := filepath.Join(codexHome, root.name)
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			files = append(files, sessionFile{
				path:     path,
				archived: root.archived,
				id:       sessionIDFromPath(path),
				info:     info,
			})
			return nil
		})
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("scan %s: %w", dir, err)
		}
		rootCount++
	}
	if rootCount == 0 {
		return nil, fmt.Errorf("no session directories found under %s", codexHome)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

func sessionIDFromPath(path string) string {
	if match := uuidPattern.FindStringSubmatch(filepath.Base(path)); len(match) > 1 {
		return strings.ToLower(match[1])
	}
	sum := sha256.Sum256([]byte(path))
	return "file-" + hex.EncodeToString(sum[:12])
}

func indexSessionFile(ctx context.Context, store *Store, file sessionFile, title, generation string) (int, error) {
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

	if _, err := tx.ExecContext(ctx, `
        INSERT INTO sessions(
            id, path, archived, title, cwd, started_at_ms, updated_at_ms,
            size, mtime_ns, scan_generation
        ) VALUES (?, ?, ?, ?, '', NULL, NULL, 0, 0, ?)
        ON CONFLICT(id) DO UPDATE SET
            path = excluded.path,
            archived = excluded.archived,
            title = excluded.title,
            scan_generation = excluded.scan_generation`,
		file.id, file.path, file.archived, title, generation); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE session_id = ?`, file.id); err != nil {
		return 0, err
	}
	insert, err := tx.PrepareContext(ctx, `
        INSERT INTO records(
			session_id, line_number, timestamp_ms, role, phase, text
		) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = insert.Close() }()

	scan := sessionScan{}
	if timestamp, ok := timestampFromFilename(file.path); ok {
		scan.startedAt = timestamp
	}
	scan, err = scanSessionLines(ctx, handle, insert, file.id, scan)
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
		SET cwd = ?, started_at_ms = ?, updated_at_ms = ?, size = ?, mtime_ns = ?,
		    line_count = ?
		WHERE id = ?`, scan.cwd, nullableUnixMilli(scan.startedAt), nullableUnixMilli(scan.updatedAt),
		scan.offset, afterInfo.ModTime().UnixNano(),
		scan.lineNumber, file.id); err != nil {
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
	title, generation string,
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
			session_id, line_number, timestamp_ms, role, phase, text
		) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = insert.Close() }()

	scan := sessionScan{offset: state.size, lineNumber: state.lastLine, cwd: state.cwd}
	if state.startedAtMS.Valid {
		scan.startedAt = time.UnixMilli(state.startedAtMS.Int64)
	}
	if state.updatedAtMS.Valid {
		scan.updatedAt = time.UnixMilli(state.updatedAtMS.Int64)
	}
	scan, err = scanSessionLines(ctx, handle, insert, file.id, scan)
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
		WHERE id = ?`, file.path, file.archived, title, scan.cwd,
		nullableUnixMilli(scan.startedAt), nullableUnixMilli(scan.updatedAt), scan.offset,
		afterInfo.ModTime().UnixNano(), scan.lineNumber, generation, file.id); err != nil {
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
	sessionID string,
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
			extracted := extractJSONLine(line)
			if scan.cwd == "" && extracted.cwd != "" {
				scan.cwd = extracted.cwd
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
				if _, err := insert.ExecContext(ctx, sessionID, scan.lineNumber, timestamp,
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

func extractJSONLine(line []byte) extractedLine {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return extractedLine{}
	}

	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return extractedLine{}
	}

	var result extractedLine
	sanitizeInjectedContext(value)
	extractMetadata("", value, 0, &result)
	result.text, result.role = conversationText(value)
	result.text = limitString(result.text)
	return result
}

func conversationText(value any) (string, string) {
	root, ok := value.(map[string]any)
	if !ok {
		return "", ""
	}
	message := root
	if payload, ok := root["payload"].(map[string]any); ok {
		if role, _ := payload["role"].(string); role == "user" || role == "assistant" {
			message = payload
		}
	}
	role, _ := message["role"].(string)
	if role != "user" && role != "assistant" {
		return "", ""
	}

	var parts []string
	for _, key := range []string{"message", "content", "text"} {
		appendConversationValue(&parts, message[key])
	}
	if len(parts) == 0 {
		appendConversationValue(&parts, root["message"])
	}
	return strings.Join(parts, "\n"), role
}

func appendConversationValue(parts *[]string, value any) {
	switch value := value.(type) {
	case string:
		value = strings.TrimSpace(value)
		if value != "" && !isBase64DataURL(value) {
			*parts = append(*parts, value)
		}
	case map[string]any:
		for _, childKey := range []string{"message", "content", "text"} {
			appendConversationValue(parts, value[childKey])
		}
	case []any:
		for _, child := range value {
			appendConversationValue(parts, child)
		}
	}
}

func isBase64DataURL(value string) bool {
	return strings.HasPrefix(value, "data:") && strings.Contains(value[:min(len(value), 128)], ";base64,")
}

func sanitizeInjectedContext(value any) {
	switch value := value.(type) {
	case map[string]any:
		filterInjectedContent(value)
		for _, child := range value {
			sanitizeInjectedContext(child)
		}
	case []any:
		for _, child := range value {
			sanitizeInjectedContext(child)
		}
	}
}

func filterInjectedContent(object map[string]any) {
	content, ok := object["content"].([]any)
	if !ok {
		return
	}
	metadata, _ := object["internal_chat_message_metadata_passthrough"].(map[string]any)
	kinds, _ := metadata["content_item_kinds"].([]any)
	kindsAlign := len(content) == len(kinds)

	filtered := make([]any, 0, len(content))
	for index, item := range content {
		if kindsAlign {
			kind, _ := kinds[index].(string)
			if !isInjectedContentKind(kind) {
				filtered = append(filtered, item)
			}
		} else if !isLegacyInjectedContentItem(item) {
			filtered = append(filtered, item)
		}
	}
	object["content"] = filtered
	delete(object, "internal_chat_message_metadata_passthrough")
}

func isLegacyInjectedContentItem(item any) bool {
	object, ok := item.(map[string]any)
	if !ok {
		return false
	}
	text, ok := object["text"].(string)
	if !ok {
		return false
	}
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "# AGENTS.md instructions for ") && strings.Contains(text, "<INSTRUCTIONS>") ||
		strings.HasPrefix(text, "<recommended_plugins>") && strings.Contains(text, "</recommended_plugins>") ||
		strings.HasPrefix(text, "<environment_context>") && strings.Contains(text, "</environment_context>")
}

func isInjectedContentKind(kind string) bool {
	switch kind {
	case "agents_md.instructions", "plugins.recommendations", "environments.environment_context":
		return true
	default:
		return false
	}
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

func timestampFromFilename(path string) (time.Time, bool) {
	match := filenamePattern.FindStringSubmatch(filepath.Base(path))
	if len(match) < 2 {
		return time.Time{}, false
	}
	value, err := time.ParseInLocation("2006-01-02T15-04-05", match[1], time.Local)
	return value, err == nil
}

func loadSessionTitles(path string) map[string]string {
	titles := make(map[string]string)
	handle, err := os.Open(path)
	if err != nil {
		return titles
	}
	defer func() { _ = handle.Close() }()

	scanner := bufio.NewScanner(handle)
	scanner.Buffer(nil, maxIndexedStringBytes)
	for scanner.Scan() {
		var entry struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.ID != "" && entry.ThreadName != "" {
			titles[entry.ID] = entry.ThreadName
		}
	}
	return titles
}
