package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebHandler(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	sessionDir := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	filename := "rollout-2026-08-29T09-00-00-" + testSessionID + ".jsonl"
	sessionPath := filepath.Join(sessionDir, filename)
	longText := strings.Repeat("full record ", 80) + "final tail marker"
	data := `{"timestamp":"2026-08-29T00:00:00Z","role":"user","message":"searchable web text <script>alert(1)</script>"}
{"timestamp":"2026-08-29T00:00:01Z","role":"system","message":"hidden system instruction"}
{"timestamp":"2026-08-29T00:00:02Z","type":"function_call_output","message":"hidden tool output"}
{"timestamp":"2026-08-29T00:00:03Z","type":"response_item","payload":{"type":"message","id":"msg_internal","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"visible assistant reply"}]}}
` + `{"timestamp":"2026-08-29T00:00:04Z","role":"assistant","message":"` + longText + `"}
`
	if err := os.WriteFile(sessionPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	indexData := `{"id":"` + testSessionID + `","thread_name":"Web session","updated_at":"2026-08-29T00:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), []byte(indexData), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := IndexSessions(context.Background(), store, SessionHomes{Codex: codexHome}); err != nil {
		t.Fatal(err)
	}
	handler := NewWebHandler(store)

	response := get(t, handler, "/api/v1/search?q="+url.QueryEscape("web visible"))
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("API content type = %q", contentType)
	}
	var apiResponse apiSearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &apiResponse); err != nil {
		t.Fatal(err)
	}
	if apiResponse.Query != "web visible" || apiResponse.HasMore || apiResponse.NextOffset != nil || len(apiResponse.Results) != 1 {
		t.Fatalf("unexpected API response: %+v", apiResponse)
	}
	result := apiResponse.Results[0]
	if result.Source != sourceCodex || result.SessionID != testSessionID || result.Path != sessionPath ||
		result.URL == nil || *result.URL != "codex://threads/"+testSessionID || result.Title != "Web session" ||
		result.MatchCount != 2 || len(result.Matches) != 2 {
		t.Fatalf("unexpected API result: %+v", result)
	}
	if result.Matches[0].LineNumber != 1 || strings.Join(result.Matches[0].MatchedTerms, " ") != "web" ||
		!strings.Contains(result.Matches[0].Snippet, "searchable web text") ||
		result.Matches[1].LineNumber != 4 || strings.Join(result.Matches[1].MatchedTerms, " ") != "visible" ||
		!strings.Contains(result.Matches[1].Snippet, "visible assistant reply") {
		t.Fatalf("unexpected API matches: %+v", result.Matches)
	}
	history, err := store.ListSearchHistory(context.Background(), searchHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("API search updated web search history: %v", history)
	}

	response = get(t, handler, "/")
	if !strings.Contains(response.Body.String(), `data-copy-text="`+sessionPath+`"`) ||
		!strings.Contains(response.Body.String(), `href="codex://threads/`+testSessionID+`"`) ||
		!strings.Contains(response.Body.String(), `>Open in Codex</a>`) ||
		!strings.Contains(response.Body.String(), `class="copy-feedback" role="status" aria-live="polite"`) ||
		!strings.Contains(response.Body.String(), `data-clear-query aria-label="Clear search" hidden`) ||
		!strings.Contains(response.Body.String(), `<link rel="icon" href="/favicon.svg" type="image/svg+xml">`) ||
		!strings.Contains(response.Body.String(), "Search local Codex and Claude sessions.") ||
		!strings.Contains(response.Body.String(), `class="badge source-badge source-codex"`) ||
		!strings.Contains(response.Body.String(), `src="/icons/openai.svg"`) ||
		!strings.Contains(response.Body.String(), `id="search-history"`) ||
		!strings.Contains(response.Body.String(), "Your recent searches will appear here.") ||
		!strings.Contains(response.Body.String(), `href="/sessions/`+sourceCodex+`/`+testSessionID+`?shorten=1"`) {
		t.Fatalf("recent session copy button missing: status=%d body=%s", response.Code, response.Body.String())
	}

	response = get(t, handler, "/?q="+url.QueryEscape("web text"))
	if !strings.Contains(response.Body.String(), "searchable <mark>web</mark> <mark>text</mark>") || !strings.Contains(response.Body.String(), "Web session") {
		t.Fatalf("search result missing: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `data-copy-text="`+sessionPath+`"`) {
		t.Fatalf("search result copy button missing: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `href="/sessions/`+sourceCodex+`/`+testSessionID+`?q=web&#43;text&amp;shorten=1"`) {
		t.Fatalf("search result did not enable record shortening: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `href="codex://threads/`+testSessionID+`"`) ||
		!strings.Contains(response.Body.String(), `>Open in Codex</a>`) ||
		!strings.Contains(response.Body.String(), `data-clear-query aria-label="Clear search"`) ||
		strings.Contains(response.Body.String(), `data-clear-query aria-label="Clear search" hidden`) {
		t.Fatalf("search result actions missing: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `href="/?q=web&#43;text&amp;from_history=1"`) {
		t.Fatalf("search history missing: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "internal=1") {
		t.Fatalf("conversation search unexpectedly enabled internal records: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "<script>alert(1)</script>") {
		t.Fatalf("record text was not escaped: %s", response.Body.String())
	}
	if response.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("security headers are missing")
	}
	if !strings.Contains(response.Body.String(), `data-live-search`) ||
		!strings.Contains(response.Body.String(), `id="results"`) ||
		!strings.Contains(response.Body.String(), `src="/app.js"`) {
		t.Fatalf("live-search markup missing: %s", response.Body.String())
	}

	get(t, handler, "/?q="+url.QueryEscape("visible assistant"))
	response = get(t, handler, "/?q="+url.QueryEscape("web text")+"&from_history=1")
	historyHTML := response.Body.String()
	visiblePosition := strings.Index(historyHTML, `>visible assistant</a>`)
	webPosition := strings.Index(historyHTML, `>web text</a>`)
	if visiblePosition < 0 || webPosition < 0 || visiblePosition > webPosition {
		t.Fatalf("history selection changed the order: %s", historyHTML)
	}
	if !strings.Contains(historyHTML, `class="history-link history-link-active" href="/?q=web&#43;text&amp;from_history=1" title="web text" aria-current="page"`) {
		t.Fatalf("selected history was not highlighted: %s", historyHTML)
	}
	if !strings.Contains(historyHTML, `href="/sessions/`+sourceCodex+`/`+testSessionID+`?q=web&#43;text&amp;shorten=1&amp;from_history=1"`) {
		t.Fatalf("history search result did not preserve navigation state: %s", historyHTML)
	}

	response = get(t, handler, "/sessions/"+sourceCodex+"/"+testSessionID)
	if !strings.Contains(response.Body.String(), "codex://threads/"+testSessionID) ||
		!strings.Contains(response.Body.String(), `>Open in Codex</a>`) {
		t.Fatalf("Codex deep link missing: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `data-clear-query aria-label="Clear filter" hidden`) {
		t.Fatalf("empty session filter clear button missing: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `data-copy-text="`+sessionPath+`"`) {
		t.Fatalf("session copy button missing: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "final tail marker") {
		t.Fatalf("direct session record was shortened: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `name="shorten" value="1"`) ||
		strings.Contains(response.Body.String(), `name="shorten" value="1" checked`) {
		t.Fatalf("shorten checkbox default is incorrect: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "max-height: 420px") {
		t.Fatalf("session records still have a fixed maximum height: %s", response.Body.String())
	}

	response = get(t, handler, "/sessions/"+sourceCodex+"/"+testSessionID+"?shorten=1")
	if strings.Contains(response.Body.String(), "final tail marker") ||
		!strings.Contains(response.Body.String(), "…") ||
		!strings.Contains(response.Body.String(), `name="shorten" value="1" checked`) {
		t.Fatalf("session records were not shortened: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "max-height: 420px") {
		t.Fatalf("shortened session records have a fixed maximum height: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `href="/?q=web&#43;text&amp;from_history=1"`) {
		t.Fatalf("session page search history missing: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "searchable web text") ||
		!strings.Contains(response.Body.String(), "visible assistant reply") ||
		strings.Contains(response.Body.String(), "msg_internal") ||
		strings.Contains(response.Body.String(), "output_text") ||
		strings.Contains(response.Body.String(), "response_item") ||
		strings.Contains(response.Body.String(), "hidden system instruction") ||
		strings.Contains(response.Body.String(), "hidden tool output") {
		t.Fatalf("session page did not filter internal records: %s", response.Body.String())
	}
	for _, badge := range []string{
		`class="badge badge-user">user`,
		`class="badge badge-assistant">assistant`,
		`class="badge badge-commentary">commentary`,
	} {
		if !strings.Contains(response.Body.String(), badge) {
			t.Fatalf("session page is missing %q: %s", badge, response.Body.String())
		}
	}

	response = get(t, handler, "/sessions/"+sourceCodex+"/"+testSessionID+"?q="+url.QueryEscape("web text")+"&from_history=1")
	if !strings.Contains(response.Body.String(), `name="from_history" value="1"`) ||
		!strings.Contains(response.Body.String(), `class="back" href="/?q=web&#43;text&amp;from_history=1"`) ||
		!strings.Contains(response.Body.String(), `class="history-link history-link-active"`) {
		t.Fatalf("history navigation state was not preserved: status=%d body=%s", response.Code, response.Body.String())
	}

	response = get(t, handler, "/sessions/"+sourceCodex+"/"+testSessionID+"?q="+url.QueryEscape("web text"))
	if !strings.Contains(response.Body.String(), "searchable <mark>web</mark> <mark>text</mark>") ||
		!strings.Contains(response.Body.String(), `data-clear-query aria-label="Clear filter"`) ||
		strings.Contains(response.Body.String(), `data-clear-query aria-label="Clear filter" hidden`) {
		t.Fatalf("session highlight missing: %s", response.Body.String())
	}

	response = get(t, handler, "/?q="+url.QueryEscape("hidden system"))
	if !strings.Contains(response.Body.String(), "No matches.") ||
		strings.Contains(response.Body.String(), "Web session") {
		t.Fatalf("internal record was globally searchable: status=%d body=%s", response.Code, response.Body.String())
	}

	response = get(t, handler, "/sessions/"+sourceCodex+"/"+testSessionID+"?q="+url.QueryEscape("hidden system"))
	if !strings.Contains(response.Body.String(), "No records.") {
		t.Fatalf("system record appeared on session page: status=%d body=%s", response.Code, response.Body.String())
	}

	response = get(t, handler, "/app.js")
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/javascript") {
		t.Fatalf("content type = %q", contentType)
	}
	for _, expected := range []string{"navigator.clipboard", `closest("[data-copy-text]")`, "copy-feedback-visible", "data-clear-query", `input.value = ""`, "AbortController", "compositionstart", "searchTerms", "form.requestSubmit()", "new FormData(form)", "shortenCheckbox", "nextHistory", "#search-history", "setTimeout(() => void runSearch(), 500)"} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("app.js is missing %q", expected)
		}
	}
	if strings.Contains(response.Body.String(), "button.textContent") {
		t.Fatalf("app.js still changes the copy button label: %s", response.Body.String())
	}

	response = get(t, handler, "/favicon.svg")
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "image/svg+xml") {
		t.Fatalf("favicon content type = %q", contentType)
	}
	if !strings.Contains(response.Body.String(), "<svg") || !strings.Contains(response.Body.String(), "#116b5b") {
		t.Fatalf("unexpected favicon: %s", response.Body.String())
	}

	response = get(t, handler, "/icons/openai.svg")
	if !strings.Contains(response.Body.String(), "<svg") || !strings.Contains(response.Body.String(), `fill="black"`) {
		t.Fatalf("unexpected OpenAI icon: %s", response.Body.String())
	}
}

func TestSearchAPIParameters(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler := NewWebHandler(store)

	response := get(t, handler, "/api/v1/search")
	var apiResponse apiSearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &apiResponse); err != nil {
		t.Fatal(err)
	}
	if apiResponse.Query != "" || apiResponse.Results == nil || len(apiResponse.Results) != 0 {
		t.Fatalf("unexpected empty search response: %+v", apiResponse)
	}

	for _, target := range []string{
		"/api/v1/search?limit=0",
		"/api/v1/search?limit=invalid",
		"/api/v1/search?offset=-1",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("GET %s: status=%d content-type=%q body=%s", target, response.Code, response.Header().Get("Content-Type"), response.Body.String())
		}
		var apiError map[string]string
		if err := json.Unmarshal(response.Body.Bytes(), &apiError); err != nil || apiError["error"] == "" {
			t.Fatalf("GET %s: error=%v response=%v", target, err, apiError)
		}
	}
}

func TestSearchAPIPagination(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sessions := []struct {
		id        string
		path      string
		cwd       string
		updatedAt int64
	}{
		{id: testSessionID, path: "/tmp/newer.jsonl", cwd: "/work/cpm/subdir", updatedAt: 2_000},
		{id: "11111111-2222-3333-4444-555555555555", path: "/tmp/older.jsonl", cwd: "/work/cpm-other", updatedAt: 1_000},
	}
	for _, session := range sessions {
		result, err := store.db.Exec(`
			INSERT INTO sessions(
				source, source_id, path, archived, title, cwd, updated_at_ms,
				size, mtime_ns, scan_generation
			) VALUES (?, ?, ?, 0, '', ?, ?, 0, 0, 'test')`,
			sourceCodex, session.id, session.path, session.cwd, session.updatedAt)
		if err != nil {
			t.Fatal(err)
		}
		sessionKey, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`
			INSERT INTO records(session_key, line_number, role, text)
			VALUES (?, 1, 'user', 'pagination match')`, sessionKey); err != nil {
			t.Fatal(err)
		}
	}

	handler := NewWebHandler(store)
	response := get(t, handler, "/api/v1/search?q=pagination&limit=1")
	var firstPage apiSearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Results) != 1 || firstPage.Results[0].SessionID != sessions[0].id ||
		!firstPage.HasMore || firstPage.NextOffset == nil || *firstPage.NextOffset != 1 {
		t.Fatalf("unexpected first page: %+v", firstPage)
	}

	response = get(t, handler, "/api/v1/search?q=pagination&limit=1&offset=1")
	var secondPage apiSearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Results) != 1 || secondPage.Results[0].SessionID != sessions[1].id ||
		secondPage.HasMore || secondPage.NextOffset != nil {
		t.Fatalf("unexpected second page: %+v", secondPage)
	}

	response = get(t, handler, "/api/v1/search?q=pagination&cwd="+url.QueryEscape("/work/cpm/"))
	var filtered apiSearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if filtered.Filters.CWD != "/work/cpm" || len(filtered.Results) != 1 ||
		filtered.Results[0].SessionID != sessions[0].id || filtered.HasMore {
		t.Fatalf("unexpected cwd-filtered response: %+v", filtered)
	}
}

func TestSearchAPIMatchOrdering(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	result, err := store.db.Exec(`
		INSERT INTO sessions(
			source, source_id, path, archived, title, cwd,
			size, mtime_ns, scan_generation
		) VALUES (?, ?, '/tmp/matches.jsonl', 0, '', '', 0, 0, 'test')`,
		sourceCodex, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	sessionKey, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []struct {
		line int
		role string
		text string
	}{
		{line: 1, role: "assistant", text: "alpha beta together"},
		{line: 2, role: "user", text: "alpha only"},
		{line: 3, role: "assistant", text: "beta only"},
	} {
		if _, err := store.db.Exec(`
			INSERT INTO records(session_key, line_number, role, text)
			VALUES (?, ?, ?, ?)`, sessionKey, record.line, record.role, record.text); err != nil {
			t.Fatal(err)
		}
	}

	response := get(t, NewWebHandler(store), "/api/v1/search?q=alpha+beta")
	var apiResponse apiSearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &apiResponse); err != nil {
		t.Fatal(err)
	}
	if len(apiResponse.Results) != 1 || len(apiResponse.Results[0].Matches) != 3 {
		t.Fatalf("unexpected response: %+v", apiResponse)
	}
	matches := apiResponse.Results[0].Matches
	if matches[0].LineNumber != 1 || strings.Join(matches[0].MatchedTerms, " ") != "alpha beta" ||
		matches[1].LineNumber != 2 || strings.Join(matches[1].MatchedTerms, " ") != "alpha" ||
		matches[2].LineNumber != 3 || strings.Join(matches[2].MatchedTerms, " ") != "beta" {
		t.Fatalf("unexpected match order: %+v", matches)
	}
}

func TestWebHandlerShowsClaudeSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	claudeHome := filepath.Join(root, ".claude")
	projectDir := filepath.Join(claudeHome, "projects", "-tmp-project")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projectDir, testSessionID+".jsonl")
	data := `{"type":"ai-title","aiTitle":"Claude web session","sessionId":"` + testSessionID + `"}
{"type":"user","sessionId":"` + testSessionID + `","cwd":"/tmp/project","timestamp":"2026-08-29T00:00:00Z","message":{"role":"user","content":"Claude searchable web text"}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := IndexSessions(context.Background(), store, SessionHomes{Claude: claudeHome}); err != nil {
		t.Fatal(err)
	}
	handler := NewWebHandler(store)

	response := get(t, handler, "/?q="+url.QueryEscape("Claude searchable"))
	body := response.Body.String()
	if !strings.Contains(body, `class="badge source-badge source-claude"`) ||
		!strings.Contains(body, `class="source-claude-dot" aria-hidden="true"`) ||
		strings.Contains(body, `/icons/claude.svg`) ||
		!strings.Contains(body, `href="/sessions/claude/`+testSessionID) ||
		!strings.Contains(body, `href="claude://resume?session=`+testSessionID+`"`) ||
		!strings.Contains(body, `>Open in Claude</a>`) {
		t.Fatalf("Claude source badge missing: %s", body)
	}
	response = get(t, handler, "/api/v1/search?q="+url.QueryEscape("Claude searchable"))
	var apiResponse apiSearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &apiResponse); err != nil {
		t.Fatal(err)
	}
	if len(apiResponse.Results) != 1 || apiResponse.Results[0].URL == nil ||
		*apiResponse.Results[0].URL != "claude://resume?session="+testSessionID {
		t.Fatalf("Claude API deep link missing: %+v", apiResponse)
	}
	response = get(t, handler, "/sessions/"+sourceClaude+"/"+testSessionID)
	if !strings.Contains(response.Body.String(), `href="claude://resume?session=`+testSessionID+`"`) ||
		!strings.Contains(response.Body.String(), `>Open in Claude</a>`) ||
		strings.Contains(response.Body.String(), "codex://threads/") {
		t.Fatalf("Claude session deep link missing: %s", response.Body.String())
	}
}

func TestSessionAppLinkRejectsInvalidID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "not-a-uuid", testSessionID + "?unexpected=true"} {
		if link := sessionAppLink(sourceCodex, id); link != nil {
			t.Fatalf("sessionAppLink(%q) = %+v", id, link)
		}
	}
	if link := sessionAppLink("unknown", testSessionID); link != nil {
		t.Fatalf("unknown source link = %+v", link)
	}
}

func get(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return response
}

func TestMakeSnippet(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("before ", 100) + "NEEDLE" + strings.Repeat(" after", 100)
	snippet := makeSnippet(text, []string{"needle"}, 80)
	if !strings.Contains(snippet, "NEEDLE") || len([]rune(snippet)) > 82 {
		t.Fatalf("unexpected snippet: %q", snippet)
	}
}
