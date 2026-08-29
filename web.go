package main

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	resultLimit        = 50
	searchHistoryLimit = 50
)

type webApp struct {
	store           *Store
	indexTemplate   *template.Template
	sessionTemplate *template.Template
}

type indexPage struct {
	Query       string
	Hits        []SearchHit
	Sessions    []Session
	Elapsed     time.Duration
	PreviousURL string
	NextURL     string
	HasMore     bool
	Searched    bool
	History     []string
	FromHistory bool
}

type sessionPage struct {
	Session     Session
	Query       string
	Records     []Record
	CodexURL    template.URL
	History     []string
	FromHistory bool
}

func NewWebHandler(store *Store) http.Handler {
	functions := template.FuncMap{
		"formatTime": unixMilliString,
		"formatSize": formatSize,
		"highlight": func(text, query string) []textPart {
			return highlightText(text, parseSearchQuery(query))
		},
		"badgeClass": badgeClass,
	}
	app := &webApp{
		store:           store,
		indexTemplate:   template.Must(template.New("index").Funcs(functions).Parse(indexHTML)),
		sessionTemplate: template.Must(template.New("session").Funcs(functions).Parse(sessionHTML)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app.js", staticHandler("text/javascript; charset=utf-8", "no-cache", appJS))
	mux.HandleFunc("GET /favicon.svg", staticHandler("image/svg+xml; charset=utf-8", "public, max-age=86400", faviconSVG))
	mux.HandleFunc("GET /", app.index)
	mux.HandleFunc("GET /sessions/{id}", app.session)
	return securityHeaders(mux)
}

func badgeClass(value string) string {
	switch strings.ToLower(value) {
	case "user":
		return "badge-user"
	case "assistant":
		return "badge-assistant"
	case "commentary":
		return "badge-commentary"
	case "final_answer":
		return "badge-final-answer"
	default:
		return ""
	}
}

func staticHandler(contentType, cacheControl, content string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", cacheControl)
		_, _ = w.Write([]byte(content))
	}
}

func (app *webApp) index(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	fromHistory := r.URL.Query().Get("from_history") == "1"
	page := indexPage{Query: query, Searched: query != "", FromHistory: fromHistory}

	started := time.Now()
	if query == "" {
		sessions, err := app.store.ListSessions(r.Context(), resultLimit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		page.Sessions = sessions
	} else {
		hits, err := app.store.Search(r.Context(), query, resultLimit+1, offset)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(hits) > resultLimit {
			page.HasMore = true
			hits = hits[:resultLimit]
		}
		page.Hits = hits
		page.NextURL = searchURL(query, offset+resultLimit, fromHistory)
		if offset > 0 {
			page.PreviousURL = searchURL(query, max(0, offset-resultLimit), fromHistory)
		}
		if offset == 0 && !fromHistory {
			if err := app.store.RecordSearch(r.Context(), query, searchHistoryLimit); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	history, err := app.store.ListSearchHistory(r.Context(), searchHistoryLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page.History = history
	page.Elapsed = time.Since(started).Round(time.Millisecond)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := app.indexTemplate.Execute(w, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (app *webApp) session(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, err := app.store.GetSession(r.Context(), id)
	if err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	fromHistory := r.URL.Query().Get("from_history") == "1"
	records, err := app.store.SessionRecords(r.Context(), id, query, 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	history, err := app.store.ListSearchHistory(r.Context(), searchHistoryLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page := sessionPage{
		Session:     session,
		Query:       query,
		Records:     records,
		History:     history,
		FromHistory: fromHistory,
	}
	if uuidPattern.MatchString(id) {
		page.CodexURL = template.URL("codex://threads/" + id)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := app.sessionTemplate.Execute(w, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func searchURL(query string, offset int, fromHistory bool) string {
	values := url.Values{"q": []string{query}}
	if offset > 0 {
		values.Set("offset", strconv.Itoa(offset))
	}
	if fromHistory {
		values.Set("from_history", "1")
	}
	return "/?" + values.Encode()
}

func formatSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	divisor := int64(unit)
	exponent := 0
	for value := size / unit; value >= unit && exponent < 3; value /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(divisor), "KMGT"[exponent])
}

const baseCSS = `
:root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, sans-serif; }
* { box-sizing: border-box; }
body { margin: 0; background: #f5f5f2; color: #20201e; }
main { width: min(1440px, calc(100% - 48px)); margin: 0 auto; padding: 40px 0 80px; }
.app-shell { display: grid; grid-template-columns: 270px minmax(0, 1fr); gap: 32px; align-items: start; }
.content-pane { min-width: 0; }
.history-pane { position: sticky; top: 24px; max-height: calc(100vh - 48px); overflow: auto; padding: 18px; border: 1px solid #d8d5cd; border-radius: 12px; background: #fff; box-shadow: 0 1px 2px rgb(0 0 0 / 4%); }
.history-pane h2 { margin: 0 0 12px; font-size: 15px; }
.history-list { display: grid; gap: 4px; }
.history-link { display: block; overflow: hidden; padding: 7px 9px; border-radius: 7px; color: #464541; font-size: 13px; line-height: 1.35; text-decoration: none; text-overflow: ellipsis; white-space: nowrap; }
.history-link:hover { background: #efeee9; color: #0c6959; }
.history-link-active, .history-link-active:hover { background: #dcece7; color: #0a6253; font-weight: 650; }
.history-empty { color: #77736c; font-size: 13px; line-height: 1.5; }
a { color: #0c6959; }
h1 { margin: 0 0 8px; font-size: 30px; letter-spacing: -0.03em; }
.subtle { color: #66645f; font-size: 14px; }
.search { display: flex; flex-wrap: wrap; gap: 8px; margin: 28px 0 6px; }
.search input { flex: 1; min-width: 0; padding: 13px 15px; border: 1px solid #c8c5bd; border-radius: 9px; background: #fff; color: #20201e; font-size: 16px; }
.live-status { min-height: 20px; margin: 0 0 20px; color: #66645f; font-size: 13px; }
button, .button { display: inline-block; padding: 11px 16px; border: 0; border-radius: 9px; background: #116b5b; color: #fff; font-weight: 650; text-decoration: none; cursor: pointer; }
button:disabled { cursor: default; opacity: 0.7; }
.button-row { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 8px; }
.button-secondary { background: #e4e9e6; color: #28554c; }
.button-small { flex: none; padding: 6px 10px; font-size: 12px; }
.card-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; }
.card-head h2 { min-width: 0; overflow-wrap: anywhere; }
.list { display: grid; gap: 12px; }
.card { padding: 18px 20px; border: 1px solid #d8d5cd; border-radius: 12px; background: #fff; box-shadow: 0 1px 2px rgb(0 0 0 / 4%); }
.card h2 { margin: 0; font-size: 17px; }
.meta { display: flex; flex-wrap: wrap; gap: 6px 14px; margin-top: 7px; color: #6b6862; font-size: 13px; }
.snippet { margin: 14px 0 0; overflow-wrap: anywhere; white-space: pre-wrap; line-height: 1.5; }
mark { padding: 0 2px; border-radius: 3px; background: #ffe169; color: #25200b; }
.badge { padding: 2px 7px; border-radius: 999px; background: #ebe9e3; color: #5d5a55; font-size: 12px; }
.badge-user { background: #e2efff; color: #185787; }
.badge-assistant { background: #e3f3e7; color: #28613c; }
.badge-commentary { background: #fff0d5; color: #81530a; }
.badge-final-answer { background: #eee7fa; color: #64418a; }
.archived { background: #f2e4d5; color: #84522a; }
.pager { display: flex; justify-content: space-between; margin-top: 22px; }
.session-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 20px; }
.session-head h1 { overflow-wrap: anywhere; }
.record { margin: 12px 0; }
.record pre { margin: 10px 0 0; max-height: 420px; overflow: auto; padding: 15px; border-radius: 8px; background: #f6f5f1; color: #24231f; white-space: pre-wrap; overflow-wrap: anywhere; font: 13px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; }
.back { display: inline-block; margin-bottom: 22px; text-decoration: none; }
.empty { padding: 40px 0; text-align: center; color: #77736c; }
@media (prefers-color-scheme: dark) {
  body { background: #191a18; color: #edede8; }
  .card, .history-pane { background: #222320; border-color: #3b3c38; }
  .search input { background: #222320; color: #edede8; border-color: #4c4d48; }
  .subtle, .meta, .live-status { color: #aaa9a2; }
  .record pre { background: #171815; color: #e8e8e2; }
  .button-secondary { background: #38423e; color: #b9e1d8; }
  .badge { background: #363732; color: #d2d1ca; }
  .badge-user { background: #203d59; color: #aed6ff; }
  .badge-assistant { background: #244331; color: #b8e4c5; }
  .badge-commentary { background: #4c381a; color: #ffd99a; }
  .badge-final-answer { background: #403050; color: #ddc5f6; }
  mark { background: #8a6c00; color: #fff4bd; }
  a { color: #66cbb6; }
  .history-link { color: #d0cfc8; }
  .history-link:hover { background: #30312d; color: #66cbb6; }
  .history-link-active, .history-link-active:hover { background: #29443d; color: #8bdecc; }
  .history-empty { color: #92918a; }
}
`

const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">
  <rect width="64" height="64" rx="14" fill="#116b5b"/>
  <circle cx="27" cy="27" r="13" fill="none" stroke="#fff" stroke-width="6"/>
  <path d="M37 37 50 50" fill="none" stroke="#fff" stroke-width="7" stroke-linecap="round"/>
</svg>`

const searchHistoryHTML = `<aside class="history-pane" id="search-history">
  <h2>Search history</h2>
  {{if .History}}
  <nav class="history-list" aria-label="Search history">
    {{range .History}}<a class="history-link{{if eq . $.Query}} history-link-active{{end}}" href="/?q={{urlquery .}}&amp;from_history=1" title="{{.}}"{{if eq . $.Query}} aria-current="page"{{end}}>{{.}}</a>{{end}}
  </nav>
  {{else}}
  <div class="history-empty">Your recent searches will appear here.</div>
  {{end}}
</aside>`

const indexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <link rel="icon" href="/favicon.svg" type="image/svg+xml">
  <title>LLM Session Search</title>
  <style>` + baseCSS + `</style>
  <script src="/app.js" defer></script>
</head>
<body>
<main class="app-shell">
` + searchHistoryHTML + `
<div class="content-pane">
  <h1>LLM Session Search</h1>
  <div class="subtle">Search local Codex session files.</div>
  <form class="search" action="/" method="get" data-live-search>
    <input name="q" value="{{.Query}}" placeholder="Search sessions" autofocus autocomplete="off">
    <button type="submit">Search</button>
  </form>
  <div class="live-status" role="status" aria-live="polite"></div>

  <section id="results">
  {{if .Searched}}
    <p class="subtle">{{len .Hits}} results on this page · {{.Elapsed}}</p>
    <div class="list">
    {{range .Hits}}
      <article class="card">
        <div class="card-head">
          <h2><a href="/sessions/{{.Session.ID}}?q={{urlquery $.Query}}{{if $.FromHistory}}&amp;from_history=1{{end}}">{{if .Session.Title}}{{.Session.Title}}{{else}}{{.Session.ID}}{{end}}</a></h2>
          <button type="button" class="button-secondary button-small" data-copy-text="{{.Session.Path}}">Copy JSONL Path</button>
        </div>
        <div class="meta">
          {{if .Session.Archived}}<span class="badge archived">archived</span>{{end}}
          {{if .Record.Role}}<span class="badge {{badgeClass .Record.Role}}">{{.Record.Role}}</span>{{end}}
          {{if .Record.Phase}}<span class="badge {{badgeClass .Record.Phase}}">{{.Record.Phase}}</span>{{end}}
          <span>{{.MatchCount}} matches</span>
          {{if .Session.UpdatedAtMS.Valid}}<span>updated {{formatTime .Session.UpdatedAtMS}}</span>{{end}}
          <span>line {{.Record.LineNumber}}</span>
        </div>
        <p class="snippet">{{range highlight .Snippet $.Query}}{{if .Match}}<mark>{{.Text}}</mark>{{else}}{{.Text}}{{end}}{{end}}</p>
      </article>
    {{else}}
      <div class="empty">No matches.</div>
    {{end}}
    </div>
    <nav class="pager">
      <span>{{if .PreviousURL}}<a href="{{.PreviousURL}}">← Previous</a>{{end}}</span>
      <span>{{if .HasMore}}<a href="{{.NextURL}}">Next →</a>{{end}}</span>
    </nav>
  {{else}}
    <p class="subtle">Recently updated sessions</p>
    <div class="list">
    {{range .Sessions}}
      <article class="card">
        <div class="card-head">
          <h2><a href="/sessions/{{.ID}}">{{if .Title}}{{.Title}}{{else}}{{.ID}}{{end}}</a></h2>
          <button type="button" class="button-secondary button-small" data-copy-text="{{.Path}}">Copy JSONL Path</button>
        </div>
        <div class="meta">
          {{if .Archived}}<span class="badge archived">archived</span>{{end}}
          {{if .StartedAtMS.Valid}}<span>started {{formatTime .StartedAtMS}}</span>{{end}}
          {{if .UpdatedAtMS.Valid}}<span>updated {{formatTime .UpdatedAtMS}}</span>{{end}}
          <span>{{formatSize .Size}}</span>
          {{if .CWD}}<span>{{.CWD}}</span>{{end}}
        </div>
      </article>
    {{else}}
      <div class="empty">No indexed sessions found.</div>
    {{end}}
    </div>
  {{end}}
  </section>
</div>
</main>
</body>
</html>`

const sessionHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <link rel="icon" href="/favicon.svg" type="image/svg+xml">
  <title>{{if .Session.Title}}{{.Session.Title}}{{else}}{{.Session.ID}}{{end}} · LLM Session Search</title>
  <style>` + baseCSS + `</style>
  <script src="/app.js" defer></script>
</head>
<body>
<main class="app-shell">
` + searchHistoryHTML + `
<div class="content-pane">
  <a class="back" href="/?q={{urlquery .Query}}{{if .FromHistory}}&amp;from_history=1{{end}}">← Search</a>
  <header class="session-head">
    <div>
      <h1>{{if .Session.Title}}{{.Session.Title}}{{else}}{{.Session.ID}}{{end}}</h1>
      <div class="meta">
        {{if .Session.Archived}}<span class="badge archived">archived</span>{{end}}
        {{if .Session.StartedAtMS.Valid}}<span>started {{formatTime .Session.StartedAtMS}}</span>{{end}}
        {{if .Session.UpdatedAtMS.Valid}}<span>updated {{formatTime .Session.UpdatedAtMS}}</span>{{end}}
        <span>{{formatSize .Session.Size}}</span>
      </div>
      <div class="subtle" style="margin-top:8px">{{.Session.ID}}{{if .Session.CWD}} · {{.Session.CWD}}{{end}}</div>
    </div>
    <div class="button-row">
      {{if .CodexURL}}<a class="button" href="{{.CodexURL}}">Open in ChatGPT</a>{{end}}
      <button type="button" class="button-secondary" data-copy-text="{{.Session.Path}}">Copy JSONL Path</button>
    </div>
  </header>

  <form class="search" action="/sessions/{{.Session.ID}}" method="get" data-live-search>
    <input name="q" value="{{.Query}}" placeholder="Filter this session" autocomplete="off">
    {{if .FromHistory}}<input type="hidden" name="from_history" value="1">{{end}}
    <button type="submit">Filter</button>
  </form>
  <div class="live-status" role="status" aria-live="polite"></div>

  <section id="results" class="list">
  {{range .Records}}
    <article class="card record">
      <div class="meta">
        <span>line {{.LineNumber}}</span>
        {{if .Role}}<span class="badge {{badgeClass .Role}}">{{.Role}}</span>{{end}}
        {{if .Phase}}<span class="badge {{badgeClass .Phase}}">{{.Phase}}</span>{{end}}
        {{if .TimestampMS.Valid}}<span>{{formatTime .TimestampMS}}</span>{{end}}
      </div>
      <pre>{{range highlight .Text $.Query}}{{if .Match}}<mark>{{.Text}}</mark>{{else}}{{.Text}}{{end}}{{end}}</pre>
    </article>
  {{else}}
    <div class="empty">No records.</div>
  {{end}}
  </section>
</div>
</main>
</body>
</html>`

const appJS = `(() => {
  async function copyText(button) {
    const text = button.dataset.copyText;
    if (!text || button.disabled) return;

    const originalLabel = button.textContent;
    button.disabled = true;
    try {
      await navigator.clipboard.writeText(text);
      button.textContent = "Copied";
    } catch (_) {
      button.textContent = "Copy failed";
    }
    window.setTimeout(() => {
      button.textContent = originalLabel;
      button.disabled = false;
    }, 1500);
  }

  document.addEventListener("click", (event) => {
    const button = event.target.closest("[data-copy-text]");
    if (button) void copyText(button);
  });

  const form = document.querySelector("form[data-live-search]");
  const input = form?.querySelector('input[name="q"]');
  const status = document.querySelector(".live-status");
  let results = document.querySelector("#results");
  let history = document.querySelector("#search-history");
  if (!form || !input || !status || !results) return;

  let timer;
  let controller;
  let composing = false;

  function cancelPending() {
    window.clearTimeout(timer);
    if (controller) controller.abort();
  }

  function searchURL(query) {
    const target = new URL(form.action, window.location.href);
    target.search = new URLSearchParams(new FormData(form));
    if (query === "") {
      target.searchParams.delete("q");
    } else {
      target.searchParams.set("q", query);
    }
    return target;
  }

  function searchTerms(query) {
    const terms = [];
    let current = "";
    let quoted = false;
    const flush = () => {
      const term = current.trim();
      if (term !== "") terms.push(term);
      current = "";
    };
    for (const character of query) {
      if (character === '"') {
        flush();
        quoted = !quoted;
      } else if (/\s/u.test(character) && !quoted) {
        flush();
      } else {
        current += character;
      }
    }
    flush();
    return terms;
  }

  async function runSearch() {
    cancelPending();
    const query = input.value.trim();
    const target = searchURL(query);
    const requestController = new AbortController();
    controller = requestController;
    status.textContent = "Searching…";
    results.setAttribute("aria-busy", "true");

    try {
      const response = await fetch(target, {
        headers: {"X-Requested-With": "llm-session-search"},
        signal: requestController.signal,
      });
      if (!response.ok) throw new Error("search request failed");
      const page = new DOMParser().parseFromString(await response.text(), "text/html");
      const nextResults = page.querySelector("#results");
      const nextHistory = page.querySelector("#search-history");
      if (!nextResults) throw new Error("search results missing");
      results.replaceWith(nextResults);
      results = nextResults;
      if (history && nextHistory) {
        history.replaceWith(nextHistory);
        history = nextHistory;
      }
      window.history.replaceState(null, "", target);
      status.textContent = "";
    } catch (error) {
      if (error.name !== "AbortError") {
        status.textContent = "Search failed. Press Enter to try again.";
      }
    } finally {
      if (controller === requestController) {
        results.removeAttribute("aria-busy");
        controller = undefined;
      }
    }
  }

  function scheduleSearch() {
    cancelPending();
    if (composing) return;

    const rawQuery = input.value;
    const query = rawQuery.trim();
    if (query === "") {
      void runSearch();
      return;
    }
    const terms = searchTerms(query);
    if (terms.length === 0 || terms.some((term) => Array.from(term).length < 3)) {
      if (controller) controller.abort();
      status.textContent = "Press Enter when a search term has one or two characters; results below have not changed.";
      return;
    }

    status.textContent = "";
    if (/\s$/u.test(rawQuery) || query.endsWith('"')) {
      void runSearch();
      return;
    }
    timer = window.setTimeout(() => void runSearch(), 500);
  }

  input.addEventListener("compositionstart", () => {
    composing = true;
    cancelPending();
    status.textContent = "";
  });
  input.addEventListener("compositionend", () => {
    composing = false;
    scheduleSearch();
  });
  input.addEventListener("input", scheduleSearch);
  input.addEventListener("keydown", (event) => {
    if (event.key === "Enter" && !event.isComposing && !composing) {
      event.preventDefault();
      cancelPending();
      form.requestSubmit();
    }
  });
  form.addEventListener("submit", cancelPending);
})();`
