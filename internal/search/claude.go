package search

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func discoverClaudeSessionFiles(claudeHome string) ([]sessionFile, bool, error) {
	projectsDir := filepath.Join(claudeHome, "projects")
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("scan %s: %w", projectsDir, err)
	}

	var files []sessionFile
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		projectDir := filepath.Join(projectsDir, project.Name())
		entries, err := os.ReadDir(projectDir)
		if err != nil {
			return nil, true, fmt.Errorf("scan %s: %w", projectDir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
				continue
			}
			path := filepath.Join(projectDir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				return nil, true, err
			}
			files = append(files, sessionFile{
				source: sourceClaude,
				path:   path,
				id:     sessionIDFromPath(path),
				info:   info,
			})
		}
	}
	return files, true, nil
}

func extractClaudeJSONLine(line []byte) extractedLine {
	value, ok := decodeJSONLine(line)
	if !ok {
		return extractedLine{}
	}
	root, ok := value.(map[string]any)
	if !ok {
		return extractedLine{}
	}

	var result extractedLine
	extractMetadata("", root, 0, &result)
	switch recordType, _ := root["type"].(string); recordType {
	case "custom-title":
		result.title, _ = root["customTitle"].(string)
		result.forceTitle = true
	case "ai-title":
		result.title, _ = root["aiTitle"].(string)
	case "user", "assistant":
		message, _ := root["message"].(map[string]any)
		role, _ := message["role"].(string)
		if role != recordType {
			return result
		}
		result.role = role
		result.text = limitString(claudeConversationText(message["content"]))
	}
	result.title = strings.TrimSpace(result.title)
	return result
}

func claudeConversationText(content any) string {
	if text, ok := content.(string); ok {
		text = cleanClaudeConversationText(text)
		if !isBase64DataURL(text) {
			return text
		}
		return ""
	}

	items, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok || object["type"] != "text" {
			continue
		}
		text, _ := object["text"].(string)
		text = cleanClaudeConversationText(text)
		if text != "" && !isBase64DataURL(text) {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func cleanClaudeConversationText(text string) string {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{
		"<local-command-caveat>",
		"<command-name>",
		"<command-message>",
		"<command-args>",
		"<local-command-stdout>",
		"<task-notification>",
	} {
		if strings.HasPrefix(text, prefix) {
			return ""
		}
	}
	for {
		removed := false
		for _, tag := range []string{
			"system-reminder",
			"ide_opened_file",
			"ide_selection",
		} {
			opening := "<" + tag + ">"
			if !strings.HasPrefix(text, opening) {
				continue
			}
			closing := "</" + tag + ">"
			end := strings.Index(text, closing)
			if end < 0 {
				return ""
			}
			text = strings.TrimSpace(text[end+len(closing):])
			removed = true
			break
		}
		if !removed {
			return text
		}
	}
}
