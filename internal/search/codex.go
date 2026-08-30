package search

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var codexFilenamePattern = regexp.MustCompile(`rollout-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2})-`)

func discoverCodexSessionFiles(codexHome string) ([]sessionFile, bool, error) {
	titles := loadCodexSessionTitles(filepath.Join(codexHome, "session_index.jsonl"))
	var files []sessionFile
	found := false
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
			id := sessionIDFromPath(path)
			files = append(files, sessionFile{
				source:     sourceCodex,
				path:       path,
				archived:   root.archived,
				id:         id,
				title:      titles[id],
				titleKnown: true,
				info:       info,
			})
			return nil
		})
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, found, fmt.Errorf("scan %s: %w", dir, err)
		}
		found = true
	}
	return files, found, nil
}

func extractCodexJSONLine(line []byte) extractedLine {
	value, ok := decodeJSONLine(line)
	if !ok {
		return extractedLine{}
	}

	var result extractedLine
	sanitizeCodexInjectedContext(value)
	extractMetadata("", value, 0, &result)
	result.text, result.role = codexConversationText(value)
	result.text = limitString(result.text)
	return result
}

func codexConversationText(value any) (string, string) {
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
		appendCodexConversationValue(&parts, message[key])
	}
	if len(parts) == 0 {
		appendCodexConversationValue(&parts, root["message"])
	}
	return strings.Join(parts, "\n"), role
}

func appendCodexConversationValue(parts *[]string, value any) {
	switch value := value.(type) {
	case string:
		value = strings.TrimSpace(value)
		if value != "" && !isBase64DataURL(value) {
			*parts = append(*parts, value)
		}
	case map[string]any:
		for _, key := range []string{"message", "content", "text"} {
			appendCodexConversationValue(parts, value[key])
		}
	case []any:
		for _, child := range value {
			appendCodexConversationValue(parts, child)
		}
	}
}

func sanitizeCodexInjectedContext(value any) {
	switch value := value.(type) {
	case map[string]any:
		filterCodexInjectedContent(value)
		for _, child := range value {
			sanitizeCodexInjectedContext(child)
		}
	case []any:
		for _, child := range value {
			sanitizeCodexInjectedContext(child)
		}
	}
}

func filterCodexInjectedContent(object map[string]any) {
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
			if !isCodexInjectedContentKind(kind) {
				filtered = append(filtered, item)
			}
		} else if !isLegacyCodexInjectedContentItem(item) {
			filtered = append(filtered, item)
		}
	}
	object["content"] = filtered
	delete(object, "internal_chat_message_metadata_passthrough")
}

func isLegacyCodexInjectedContentItem(item any) bool {
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

func isCodexInjectedContentKind(kind string) bool {
	switch kind {
	case "agents_md.instructions", "plugins.recommendations", "environments.environment_context":
		return true
	default:
		return false
	}
}

func timestampFromFilename(path string) (time.Time, bool) {
	match := codexFilenamePattern.FindStringSubmatch(filepath.Base(path))
	if len(match) < 2 {
		return time.Time{}, false
	}
	value, err := time.ParseInLocation("2006-01-02T15-04-05", match[1], time.Local)
	return value, err == nil
}

func loadCodexSessionTitles(path string) map[string]string {
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
	if scanner.Err() != nil {
		return map[string]string{}
	}
	return titles
}
