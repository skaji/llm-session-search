package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testSessionID  = "01a04c8c-23cc-71c1-91c3-c32aac6ef791"
	testArchivedID = "019f4fa9-88da-7fd3-b18c-130893186a5f"
)

func TestIndexCodexSessions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dbDir := filepath.Join(root, ".llm-session-search")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(root, ".codex")
	sessionDir := filepath.Join(codexHome, "sessions", "2026", "08", "29")
	archivedDir := filepath.Join(codexHome, "archived_sessions")
	for _, dir := range []string{sessionDir, archivedDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	sessionPath := filepath.Join(sessionDir, "rollout-2026-08-29T09-00-00-"+testSessionID+".jsonl")
	sessionData := `{"timestamp":"2026-08-29T00:00:00Z","type":"session_meta","payload":{"id":"` + testSessionID + `","cwd":"/tmp/project"}}
{"timestamp":"2026-08-29T00:01:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"日本語の会話を検索したい"}]}}
{"timestamp":"2026-08-29T00:01:30Z","message":"会話を検索する重複イベント"}
{"timestamp":"2026-08-29T00:02:00Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"SQLiteで検索できます"}],"encrypted_content":"日本語の秘密"}}
{"timestamp":"2026-08-29T00:02:10Z","role":"user","message":"tinyenv appears here"}
{"timestamp":"2026-08-29T00:02:20Z","role":"assistant","message":"github appears elsewhere"}
`
	if err := os.WriteFile(sessionPath, []byte(sessionData), 0o600); err != nil {
		t.Fatal(err)
	}
	archivedPath := filepath.Join(archivedDir, "rollout-2026-07-11T14-32-18-"+testArchivedID+".jsonl")
	if err := os.WriteFile(archivedPath, []byte(`{"record_type":"message","timestamp":"2026-07-11T05:32:18Z","role":"assistant","content":[{"type":"output_text","text":"legacy Go session uses tinyenv github together"}]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexData := `{"id":"` + testSessionID + `","thread_name":"Session search","updated_at":"2026-08-29T00:02:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), []byte(indexData), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(filepath.Join(dbDir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stats, err := IndexSessions(context.Background(), store, SessionHomes{Codex: codexHome})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 2 || stats.Changed != 2 || stats.Records != 5 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	hits, err := store.Search(context.Background(), "会話を検索", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if hits[0].ID != testSessionID || hits[0].Title != "Session search" {
		t.Fatalf("unexpected hit: %+v", hits[0])
	}
	if hits[0].MatchCount != 1 {
		t.Fatalf("match count = %d, want 1", hits[0].MatchCount)
	}
	if !hits[0].StartedAtMS.Valid || !hits[0].UpdatedAtMS.Valid {
		t.Fatalf("timestamps were not indexed: %+v", hits[0].Session)
	}
	if hits[0].UpdatedAtMS.Int64 != 1787961740000 {
		t.Fatalf("updated_at_ms = %d", hits[0].UpdatedAtMS.Int64)
	}

	secretHits, err := store.Search(context.Background(), "日本語の秘密", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(secretHits) != 0 {
		t.Fatalf("encrypted content was indexed: %+v", secretHits)
	}

	shortHits, err := store.Search(context.Background(), "Go", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(shortHits) != 1 || !shortHits[0].Archived {
		t.Fatalf("short query did not find archived session: %+v", shortHits)
	}

	andHits, err := store.Search(context.Background(), "tinyenv github", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(andHits) != 2 {
		t.Fatalf("space-separated terms did not match both sessions: %+v", andHits)
	}
	phraseHits, err := store.Search(context.Background(), `"tinyenv github"`, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(phraseHits) != 1 || phraseHits[0].ID != testArchivedID {
		t.Fatalf("quoted phrase was not kept together: %+v", phraseHits)
	}
	records, err := store.SessionRecords(context.Background(), mustSessionKey(t, store, sourceCodex, testSessionID), "tinyenv github", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("session-level AND did not return matching records: %+v", records)
	}
	conversation, err := store.SessionRecords(context.Background(), mustSessionKey(t, store, sourceCodex, testSessionID), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversation) != 4 {
		t.Fatalf("session page records = %d, want 4 user/assistant records: %+v", len(conversation), conversation)
	}
	for _, record := range conversation {
		if record.Role != "user" && record.Role != "assistant" {
			t.Fatalf("non-conversation record was returned: %+v", record)
		}
	}
	stats, err = IndexSessions(context.Background(), store, SessionHomes{Codex: codexHome})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Changed != 0 || stats.Unchanged != 2 {
		t.Fatalf("unchanged files were reindexed: %+v", stats)
	}
	movedSessionPath := filepath.Join(archivedDir, filepath.Base(sessionPath))
	if err := os.Rename(sessionPath, movedSessionPath); err != nil {
		t.Fatal(err)
	}
	sessionPath = movedSessionPath
	stats, err = IndexSessions(context.Background(), store, SessionHomes{Codex: codexHome})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Changed != 1 || stats.Unchanged != 1 {
		t.Fatalf("moved session was not reindexed: %+v", stats)
	}
	movedSession, err := store.GetSession(context.Background(), sourceCodex, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if movedSession.Path != movedSessionPath || !movedSession.Archived {
		t.Fatalf("moved session path was not refreshed: %+v", movedSession)
	}

	handle, err := os.OpenFile(sessionPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.WriteString(`{"timestamp":"2026-08-29T00:02:50Z","type":"event_msg","message":"ignored append event"}` + "\n" +
		`{"timestamp":"2026-08-29T00:03:00Z","role":"user","message":"newly appended text"}` + "\n"); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	stats, err = IndexSessions(context.Background(), store, SessionHomes{Codex: codexHome})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Changed != 1 || stats.Unchanged != 1 || stats.Records != 1 {
		t.Fatalf("changed file was not reindexed: %+v", stats)
	}
	appended, err := store.SessionRecords(context.Background(), mustSessionKey(t, store, sourceCodex, testSessionID), "newly appended", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(appended) != 1 || appended[0].LineNumber != 8 {
		t.Fatalf("appended record line number was not preserved: %+v", appended)
	}

	if err := os.Remove(archivedPath); err != nil {
		t.Fatal(err)
	}
	stats, err = IndexSessions(context.Background(), store, SessionHomes{Codex: codexHome})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 1 {
		t.Fatalf("removed session was still discovered: %+v", stats)
	}
	sessions, err := store.ListSessions(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != testSessionID {
		t.Fatalf("stale session was not removed: %+v", sessions)
	}
}

func TestIndexClaudeCodeSessions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	codexSessions := filepath.Join(codexHome, "sessions")
	claudeHome := filepath.Join(root, ".claude")
	claudeProject := filepath.Join(claudeHome, "projects", "-tmp-project")
	for _, dir := range []string{codexSessions, claudeProject} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	codexPath := filepath.Join(codexSessions, "rollout-2026-08-29T09-00-00-"+testSessionID+".jsonl")
	if err := os.WriteFile(codexPath, []byte(`{"timestamp":"2026-08-29T00:00:00Z","role":"user","message":"Codex uses the same UUID"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(claudeProject, testSessionID+".jsonl")
	claudeData := `{"type":"queue-operation","operation":"enqueue","content":"queued duplicate","sessionId":"` + testSessionID + `","timestamp":"2026-08-29T01:00:00Z"}
{"type":"ai-title","aiTitle":"Generated Claude title","sessionId":"` + testSessionID + `"}
{"type":"user","sessionId":"` + testSessionID + `","uuid":"user-1","cwd":"/tmp/project","timestamp":"2026-08-29T01:00:01Z","message":{"role":"user","content":"Claude human prompt"}}
{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"assistant-thinking","timestamp":"2026-08-29T01:00:02Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private reasoning secret"}]}}
{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"assistant-tool","timestamp":"2026-08-29T01:00:03Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{"path":"tool input secret"}}]}}
{"type":"user","sessionId":"` + testSessionID + `","uuid":"tool-result","timestamp":"2026-08-29T01:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"tool output secret"}]}}
{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"assistant-text","timestamp":"2026-08-29T01:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"Visible Claude reply"}]}}
{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"assistant-reminder","timestamp":"2026-08-29T01:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"<system-reminder>assistant reminder secret</system-reminder>"}]}}
{"type":"user","sessionId":"` + testSessionID + `","uuid":"user-2","timestamp":"2026-08-29T01:00:06Z","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>user reminder secret</system-reminder>\nClaude follow-up"},{"type":"tool_result","tool_use_id":"tool-2","content":"mixed tool secret"}]}}
{"type":"user","sessionId":"` + testSessionID + `","uuid":"local-command","timestamp":"2026-08-29T01:00:07Z","message":{"role":"user","content":"<command-name>/clear</command-name><command-args>command secret</command-args>"}}
{"type":"custom-title","customTitle":"Named Claude session","sessionId":"` + testSessionID + `"}
{"type":"ai-title","aiTitle":"Later generated title","sessionId":"` + testSessionID + `"}
`
	if err := os.WriteFile(claudePath, []byte(claudeData), 0o600); err != nil {
		t.Fatal(err)
	}
	subagents := filepath.Join(claudeProject, testSessionID, "subagents")
	if err := os.MkdirAll(subagents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subagents, "agent-internal.jsonl"), []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"subagent secret"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stats, err := IndexSessions(context.Background(), store, SessionHomes{Codex: codexHome, ClaudeCode: claudeHome})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 2 || stats.Records != 4 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	codex, err := store.GetSession(context.Background(), sourceCodex, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	claude, err := store.GetSession(context.Background(), sourceClaudeCode, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if codex.Key == claude.Key {
		t.Fatalf("sources shared an internal key: codex=%+v claude=%+v", codex, claude)
	}
	if claude.Title != "Named Claude session" || claude.CWD != "/tmp/project" {
		t.Fatalf("unexpected Claude metadata: %+v", claude)
	}
	records, err := store.SessionRecords(context.Background(), claude.Key, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("Claude conversation records = %d, want 3: %+v", len(records), records)
	}
	for _, query := range []string{"queued duplicate", "private reasoning secret", "tool input secret", "tool output secret", "mixed tool secret", "assistant reminder secret", "user reminder secret", "command secret", "subagent secret"} {
		hits, err := store.Search(context.Background(), query, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 {
			t.Fatalf("internal Claude content %q was indexed: %+v", query, hits)
		}
	}
	hits, err := store.Search(context.Background(), "Visible Claude reply", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Source != sourceClaudeCode {
		t.Fatalf("Claude reply was not searchable: %+v", hits)
	}

	handle, err := os.OpenFile(claudePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.WriteString(`{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"assistant-appended","timestamp":"2026-08-29T01:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Appended Claude reply"}]}}` + "\n" +
		`{"type":"custom-title","customTitle":"Renamed Claude session","sessionId":"` + testSessionID + `"}` + "\n"); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	stats, err = IndexSessions(context.Background(), store, SessionHomes{Codex: codexHome, ClaudeCode: claudeHome})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Changed != 1 || stats.Unchanged != 1 || stats.Records != 1 {
		t.Fatalf("Claude append was not indexed incrementally: %+v", stats)
	}
	claude, err = store.GetSession(context.Background(), sourceClaudeCode, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if claude.Title != "Renamed Claude session" {
		t.Fatalf("Claude title was not updated: %+v", claude)
	}
	hits, err = store.Search(context.Background(), "Appended Claude reply", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Source != sourceClaudeCode {
		t.Fatalf("appended Claude reply was not searchable: %+v", hits)
	}
}

func TestExtractCodexJSONLineMetadata(t *testing.T) {
	t.Parallel()
	extracted := extractCodexJSONLine([]byte(`{"message":"needle","timestamp":"2026-08-29T12:34:56Z","payload":{"role":"assistant","phase":"commentary","cwd":"/work","result":{"created_at":"2023-01-01T00:00:00Z"}}}`))
	if extracted.text == "" || extracted.role != "assistant" || extracted.phase != "commentary" || extracted.cwd != "/work" {
		t.Fatalf("unexpected extraction: %+v", extracted)
	}
	if len(extracted.timestamps) != 1 {
		t.Fatalf("timestamps = %v", extracted.timestamps)
	}
}

func TestExtractCodexJSONLineUsesConversationBody(t *testing.T) {
	t.Parallel()
	extracted := extractCodexJSONLine([]byte(`{
        "timestamp":"2026-08-10T17:03:42.779Z",
        "type":"response_item",
        "payload":{
            "type":"message",
            "id":"msg_0a5c1739c5e6a421016a7a046b60248191b778ff9282366677",
            "role":"assistant",
            "content":[{"type":"output_text","text":"actual assistant reply"}],
            "phase":"final_answer"
        }
    }`))
	if extracted.text != "actual assistant reply" {
		t.Fatalf("conversation text = %q", extracted.text)
	}
	if extracted.role != "assistant" || extracted.phase != "final_answer" {
		t.Fatalf("conversation metadata was lost: %+v", extracted)
	}
	if len(extracted.timestamps) != 1 {
		t.Fatalf("conversation timestamp was lost: %v", extracted.timestamps)
	}
}

func TestExtractCodexJSONLineExcludesInjectedContext(t *testing.T) {
	t.Parallel()
	line := []byte(`{
        "timestamp":"2026-08-29T12:34:56Z",
        "type":"response_item",
        "payload":{
            "type":"message",
            "role":"user",
            "content":[
                {"type":"input_text","text":"automatic AGENTS secret"},
                {"type":"input_text","text":"actual user request"},
                {"type":"input_text","text":"automatic environment secret"},
                {"type":"input_text","text":"data:text/plain;base64,YXV0b21hdGljIGJsb2I="},
                {"type":"input_text","text":"<recommended_plugins>manually pasted</recommended_plugins>"}
            ],
            "internal_chat_message_metadata_passthrough":{
                "content_item_kinds":[
                    "agents_md.instructions",
                    "user.text",
                    "environments.environment_context",
                    "user.text",
                    "user.text"
                ]
            }
        }
    }`)
	extracted := extractCodexJSONLine(line)
	if !strings.Contains(extracted.text, "actual user request") {
		t.Fatalf("user content was excluded: %q", extracted.text)
	}
	if !strings.Contains(extracted.text, "manually pasted") {
		t.Fatalf("user text resembling injected context was excluded: %q", extracted.text)
	}
	if strings.Contains(extracted.text, "automatic AGENTS secret") ||
		strings.Contains(extracted.text, "automatic environment secret") ||
		strings.Contains(extracted.text, "base64") ||
		strings.Contains(extracted.text, "agents_md.instructions") {
		t.Fatalf("injected context was indexed: %q", extracted.text)
	}

	onlyInjected := extractCodexJSONLine([]byte(`{
        "timestamp":"2026-08-29T12:34:56Z",
        "type":"response_item",
        "payload":{
            "type":"message",
            "role":"user",
            "content":[{"type":"input_text","text":"automatic AGENTS secret"}],
            "internal_chat_message_metadata_passthrough":{
                "content_item_kinds":["agents_md.instructions"]
            }
        }
    }`))
	if onlyInjected.text != "" {
		t.Fatalf("empty injected message was retained: %q", onlyInjected.text)
	}
	if len(onlyInjected.timestamps) != 1 {
		t.Fatalf("empty injected message lost its timestamp: %v", onlyInjected.timestamps)
	}

	manual := extractCodexJSONLine([]byte(`{"role":"user","message":"I opened AGENTS.md manually"}`))
	if !strings.Contains(manual.text, "AGENTS.md manually") {
		t.Fatalf("ordinary AGENTS.md mention was excluded: %q", manual.text)
	}

	legacy := extractCodexJSONLine([]byte(`{
        "payload":{
            "role":"user",
            "content":[
                {"type":"input_text","text":"# AGENTS.md instructions for /work\n\n<INSTRUCTIONS>\nautomatic legacy secret\n</INSTRUCTIONS>"},
                {"type":"input_text","text":"<environment_context>\nautomatic legacy environment\n</environment_context>"},
                {"type":"input_text","text":"actual legacy user request"}
            ]
        }
    }`))
	if !strings.Contains(legacy.text, "actual legacy user request") ||
		strings.Contains(legacy.text, "automatic legacy secret") ||
		strings.Contains(legacy.text, "automatic legacy environment") {
		t.Fatalf("legacy injected context was not filtered: %q", legacy.text)
	}
}

func TestCodexTimestampFromFilename(t *testing.T) {
	t.Parallel()
	timestamp, ok := timestampFromFilename("rollout-2026-08-29T17-04-07-" + testSessionID + ".jsonl")
	if !ok {
		t.Fatal("timestamp was not parsed")
	}
	if got := timestamp.Format("2006-01-02 15:04:05"); got != "2026-08-29 17:04:07" {
		t.Fatalf("timestamp = %s", got)
	}
}

func TestIndexSessionsRetriesIncompleteLastLine(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	sessionDir := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessionDir, "rollout-2026-08-29T09-00-00-"+testSessionID+".jsonl")
	complete := `{"timestamp":"2026-08-29T00:00:00Z","role":"user","message":"complete"}` + "\n"
	partial := `{"timestamp":"2026-08-29T00:01:00Z","role":"user","message":"partial`
	if err := os.WriteFile(path, []byte(complete+partial), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stats, err := IndexSessions(context.Background(), store, SessionHomes{Codex: codexHome})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 1 {
		t.Fatalf("incomplete line was indexed: %+v", stats)
	}
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.WriteString(` text"}` + "\n"); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	stats, err = IndexSessions(context.Background(), store, SessionHomes{Codex: codexHome})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 1 {
		t.Fatalf("completed line was not appended: %+v", stats)
	}
	hits, err := store.Search(context.Background(), "partial text", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("completed line was not searchable: %+v", hits)
	}
}

func mustSessionKey(t *testing.T, store *Store, source, id string) int64 {
	t.Helper()
	session, err := store.GetSession(context.Background(), source, id)
	if err != nil {
		t.Fatal(err)
	}
	return session.Key
}
