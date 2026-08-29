package main

import (
	"context"
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
	data := `{"timestamp":"2026-08-29T00:00:00Z","role":"user","message":"searchable web text <script>alert(1)</script>"}
{"timestamp":"2026-08-29T00:00:01Z","role":"system","message":"hidden system instruction"}
{"timestamp":"2026-08-29T00:00:02Z","type":"function_call_output","message":"hidden tool output"}
{"timestamp":"2026-08-29T00:00:03Z","type":"response_item","payload":{"type":"message","id":"msg_internal","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"visible assistant reply"}]}}
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
	if _, err := IndexSessions(context.Background(), store, codexHome); err != nil {
		t.Fatal(err)
	}
	handler := NewWebHandler(store)

	response := get(t, handler, "/")
	if !strings.Contains(response.Body.String(), `data-copy-text="`+sessionPath+`"`) ||
		!strings.Contains(response.Body.String(), `<link rel="icon" href="/favicon.svg" type="image/svg+xml">`) ||
		!strings.Contains(response.Body.String(), `id="search-history"`) ||
		!strings.Contains(response.Body.String(), "Your recent searches will appear here.") {
		t.Fatalf("recent session copy button missing: status=%d body=%s", response.Code, response.Body.String())
	}

	response = get(t, handler, "/?q="+url.QueryEscape("web text"))
	if !strings.Contains(response.Body.String(), "searchable <mark>web</mark> <mark>text</mark>") || !strings.Contains(response.Body.String(), "Web session") {
		t.Fatalf("search result missing: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `data-copy-text="`+sessionPath+`"`) {
		t.Fatalf("search result copy button missing: %s", response.Body.String())
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

	response = get(t, handler, "/sessions/"+testSessionID)
	if !strings.Contains(response.Body.String(), "codex://threads/"+testSessionID) {
		t.Fatalf("Codex deep link missing: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `data-copy-text="`+sessionPath+`"`) {
		t.Fatalf("session copy button missing: %s", response.Body.String())
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

	response = get(t, handler, "/sessions/"+testSessionID+"?q="+url.QueryEscape("web text")+"&from_history=1")
	if !strings.Contains(response.Body.String(), `name="from_history" value="1"`) ||
		!strings.Contains(response.Body.String(), `class="back" href="/?q=web&#43;text&amp;from_history=1"`) ||
		!strings.Contains(response.Body.String(), `class="history-link history-link-active"`) {
		t.Fatalf("history navigation state was not preserved: status=%d body=%s", response.Code, response.Body.String())
	}

	response = get(t, handler, "/sessions/"+testSessionID+"?q="+url.QueryEscape("web text"))
	if !strings.Contains(response.Body.String(), "searchable <mark>web</mark> <mark>text</mark>") {
		t.Fatalf("session highlight missing: %s", response.Body.String())
	}

	response = get(t, handler, "/?q="+url.QueryEscape("hidden system"))
	if !strings.Contains(response.Body.String(), "No matches.") ||
		strings.Contains(response.Body.String(), "Web session") {
		t.Fatalf("internal record was globally searchable: status=%d body=%s", response.Code, response.Body.String())
	}

	response = get(t, handler, "/sessions/"+testSessionID+"?q="+url.QueryEscape("hidden system"))
	if !strings.Contains(response.Body.String(), "No records.") {
		t.Fatalf("system record appeared on session page: status=%d body=%s", response.Code, response.Body.String())
	}

	response = get(t, handler, "/app.js")
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/javascript") {
		t.Fatalf("content type = %q", contentType)
	}
	for _, expected := range []string{"navigator.clipboard", `closest("[data-copy-text]")`, "AbortController", "compositionstart", "searchTerms", "form.requestSubmit()", "new FormData(form)", "nextHistory", "#search-history", "setTimeout(() => void runSearch(), 500)"} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("app.js is missing %q", expected)
		}
	}

	response = get(t, handler, "/favicon.svg")
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "image/svg+xml") {
		t.Fatalf("favicon content type = %q", contentType)
	}
	if !strings.Contains(response.Body.String(), "<svg") || !strings.Contains(response.Body.String(), "#116b5b") {
		t.Fatalf("unexpected favicon: %s", response.Body.String())
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
