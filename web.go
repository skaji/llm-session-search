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
	History     []string
	FromHistory bool
}

type appLink struct {
	URL   template.URL
	Label string
}

func NewWebHandler(store *Store) http.Handler {
	functions := template.FuncMap{
		"formatTime":     unixMilliString,
		"formatSize":     formatSize,
		"sourceName":     sourceName,
		"sessionAppLink": sessionAppLink,
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
	mux.HandleFunc("GET /icons/openai.svg", staticHandler("image/svg+xml; charset=utf-8", "public, max-age=86400", openAIIconSVG))
	mux.HandleFunc("GET /api/v1/search", app.apiSearch)
	mux.HandleFunc("GET /", app.index)
	mux.HandleFunc("GET /sessions/{source}/{id}", app.session)
	return securityHeaders(mux)
}

func sourceName(source string) string {
	switch source {
	case sourceCodex:
		return "Codex"
	case sourceClaude:
		return "Claude"
	default:
		return source
	}
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
	source := r.PathValue("source")
	id := r.PathValue("id")
	session, err := app.store.GetSession(r.Context(), source, id)
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
	records, err := app.store.SessionRecords(r.Context(), session.Key, query, 500)
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := app.sessionTemplate.Execute(w, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func sessionAppLink(source, id string) *appLink {
	if matchedID := uuidPattern.FindString(id); matchedID == "" || matchedID != id {
		return nil
	}
	switch source {
	case sourceCodex:
		return &appLink{
			URL:   template.URL("codex://threads/" + id),
			Label: "Open in Codex",
		}
	case sourceClaude:
		return &appLink{
			URL:   template.URL("claude://resume?session=" + id),
			Label: "Open in Claude",
		}
	default:
		return nil
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
.query-field { position: relative; flex: 1; min-width: 0; }
.search input { width: 100%; padding: 13px 44px 13px 15px; border: 1px solid #c8c5bd; border-radius: 9px; background: #fff; color: #20201e; font-size: 16px; }
.query-clear { position: absolute; top: 50%; right: 8px; width: 32px; height: 32px; padding: 0; transform: translateY(-50%); border-radius: 999px; background: transparent; color: #77736c; font-size: 22px; font-weight: 400; line-height: 1; }
.query-clear:hover { background: #ebe9e3; color: #20201e; }
.query-clear[hidden] { display: none; }
.live-status { min-height: 20px; margin: 0 0 20px; color: #66645f; font-size: 13px; }
button, .button { display: inline-block; padding: 11px 16px; border: 0; border-radius: 9px; background: #116b5b; color: #fff; font-weight: 650; text-decoration: none; cursor: pointer; }
button:disabled { cursor: default; opacity: 0.7; }
.button-row { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 8px; }
.button-secondary { background: #e4e9e6; color: #28554c; }
.button-small { flex: none; padding: 6px 10px; font-size: 12px; }
.card-actions { display: flex; flex: none; flex-wrap: wrap; justify-content: flex-end; gap: 8px; }
.copy-control { position: relative; display: inline-flex; }
.copy-feedback { position: absolute; right: 0; bottom: calc(100% + 8px); z-index: 1; padding: 5px 8px; border-radius: 6px; background: #252622; color: #fff; font-size: 12px; font-weight: 650; line-height: 1.2; opacity: 0; pointer-events: none; transform: translateY(3px); transition: opacity 120ms ease, transform 120ms ease; white-space: nowrap; }
.copy-feedback::after { position: absolute; top: 100%; right: 14px; border: 5px solid transparent; border-top-color: #252622; content: ""; }
.copy-feedback-visible { opacity: 1; transform: translateY(0); }
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
.source-badge { display: inline-flex; align-items: center; gap: 5px; padding-left: 3px; font-weight: 650; }
.source-icon-tile { display: inline-grid; width: 18px; height: 18px; place-items: center; border-radius: 999px; background: #fff; }
.source-icon { display: block; width: 13px; height: 13px; }
.source-claude-dot { width: 12px; height: 12px; border-radius: 50%; background: #d97757; }
.source-claude { background: #f8e6df; color: #85452f; }
.archived { background: #f2e4d5; color: #84522a; }
.pager { display: flex; justify-content: space-between; margin-top: 22px; }
.session-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 20px; }
.session-head h1 { overflow-wrap: anywhere; }
.record { margin: 12px 0; }
.record pre { margin: 10px 0 0; max-height: 420px; overflow: auto; padding: 15px; border-radius: 8px; background: #f6f5f1; color: #24231f; white-space: pre-wrap; overflow-wrap: anywhere; font: 13px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; }
.back { display: inline-block; margin-bottom: 22px; text-decoration: none; }
.empty { padding: 40px 0; text-align: center; color: #77736c; }
@media (max-width: 860px) {
  main { width: min(100% - 32px, 720px); padding-top: 24px; }
  .app-shell { grid-template-columns: minmax(0, 1fr); gap: 24px; }
  .history-pane { position: static; max-height: 240px; }
  .session-head, .card-head { flex-direction: column; }
  .button-row, .card-actions { justify-content: flex-start; }
}
@media (max-width: 520px) {
  .search { flex-direction: column; }
  .search > button { width: 100%; }
}
@media (prefers-color-scheme: dark) {
  body { background: #191a18; color: #edede8; }
  .card, .history-pane { background: #222320; border-color: #3b3c38; }
  .search input { background: #222320; color: #edede8; border-color: #4c4d48; }
  .query-clear { color: #aaa9a2; }
  .query-clear:hover { background: #363732; color: #edede8; }
  .subtle, .meta, .live-status { color: #aaa9a2; }
  .record pre { background: #171815; color: #e8e8e2; }
  .button-secondary { background: #38423e; color: #b9e1d8; }
  .badge { background: #363732; color: #d2d1ca; }
  .badge-user { background: #203d59; color: #aed6ff; }
  .badge-assistant { background: #244331; color: #b8e4c5; }
  .badge-commentary { background: #4c381a; color: #ffd99a; }
  .badge-final-answer { background: #403050; color: #ddc5f6; }
  .source-codex { background: #3b3c38; color: #edede8; }
  .source-claude { background: #523126; color: #f1b39d; }
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

const openAIIconSVG = `<svg width="721" height="721" viewBox="0 0 721 721" fill="none" xmlns="http://www.w3.org/2000/svg">
<g clip-path="url(#clip0_1637_2934)"><g clip-path="url(#clip1_1637_2934)"><path d="M304.246 294.611V249.028C304.246 245.189 305.687 242.309 309.044 240.392L400.692 187.612C413.167 180.415 428.042 177.058 443.394 177.058C500.971 177.058 537.44 221.682 537.44 269.182C537.44 272.54 537.44 276.379 536.959 280.218L441.954 224.558C436.197 221.201 430.437 221.201 424.68 224.558L304.246 294.611ZM518.245 472.145V363.224C518.245 356.505 515.364 351.707 509.608 348.349L389.174 278.296L428.519 255.743C431.877 253.826 434.757 253.826 438.115 255.743L529.762 308.523C556.154 323.879 573.905 356.505 573.905 388.171C573.905 424.636 552.315 458.225 518.245 472.141V472.145ZM275.937 376.182L236.592 353.152C233.235 351.235 231.794 348.354 231.794 344.515V238.956C231.794 187.617 271.139 148.749 324.4 148.749C344.555 148.749 363.264 155.468 379.102 167.463L284.578 222.164C278.822 225.521 275.942 230.319 275.942 237.039V376.186L275.937 376.182ZM360.626 425.122L304.246 393.455V326.283L360.626 294.616L417.002 326.283V393.455L360.626 425.122ZM396.852 570.989C376.698 570.989 357.989 564.27 342.151 552.276L436.674 497.574C442.431 494.217 445.311 489.419 445.311 482.699V343.552L485.138 366.582C488.495 368.499 489.936 371.379 489.936 375.219V480.778C489.936 532.117 450.109 570.985 396.852 570.985V570.989ZM283.134 463.99L191.486 411.211C165.094 395.854 147.343 363.229 147.343 331.562C147.343 294.616 169.415 261.509 203.48 247.593V356.991C203.48 363.71 206.361 368.508 212.117 371.866L332.074 441.437L292.729 463.99C289.372 465.907 286.491 465.907 283.134 463.99ZM277.859 542.68C223.639 542.68 183.813 501.895 183.813 451.514C183.813 447.675 184.294 443.836 184.771 439.997L279.295 494.698C285.051 498.056 290.812 498.056 296.568 494.698L417.002 425.127V470.71C417.002 474.549 415.562 477.429 412.204 479.346L320.557 532.126C308.081 539.323 293.206 542.68 277.854 542.68H277.859ZM396.852 599.776C454.911 599.776 503.37 558.513 514.41 503.812C568.149 489.896 602.696 439.515 602.696 388.176C602.696 354.587 588.303 321.962 562.392 298.45C564.791 288.373 566.231 278.296 566.231 268.224C566.231 199.611 510.571 148.267 446.274 148.267C433.322 148.267 420.846 150.184 408.37 154.505C386.775 133.392 357.026 119.958 324.4 119.958C266.342 119.958 217.883 161.22 206.843 215.921C153.104 229.837 118.557 280.218 118.557 331.557C118.557 365.146 132.95 397.771 158.861 421.283C156.462 431.36 155.022 441.437 155.022 451.51C155.022 520.123 210.682 571.466 274.978 571.466C287.931 571.466 300.407 569.549 312.883 565.228C334.473 586.341 364.222 599.776 396.852 599.776Z" fill="black"/></g></g>
<defs><clipPath id="clip0_1637_2934"><rect width="720" height="720" fill="white" transform="translate(0.606934 0.0999756)"/></clipPath><clipPath id="clip1_1637_2934"><rect width="484.139" height="479.818" fill="white" transform="translate(118.557 119.958)"/></clipPath></defs>
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

const sourceBadgeHTML = `<span class="badge source-badge source-{{.Source}}">
  <span class="source-icon-tile">{{if eq .Source "claude"}}<span class="source-claude-dot" aria-hidden="true"></span>{{else}}<img class="source-icon" src="/icons/openai.svg" alt="">{{end}}</span>
  {{sourceName .Source}}
</span>`

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
  <div class="subtle">Search local Codex and Claude sessions.</div>
  <form class="search" action="/" method="get" data-live-search>
    <div class="query-field">
      <input name="q" value="{{.Query}}" placeholder="Search sessions" autofocus autocomplete="off">
      <button type="button" class="query-clear" data-clear-query aria-label="Clear search"{{if not .Query}} hidden{{end}}>×</button>
    </div>
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
          <h2><a href="/sessions/{{.Session.Source}}/{{.Session.ID}}?q={{urlquery $.Query}}{{if $.FromHistory}}&amp;from_history=1{{end}}">{{if .Session.Title}}{{.Session.Title}}{{else}}{{.Session.ID}}{{end}}</a></h2>
          <div class="card-actions">
            {{with sessionAppLink .Session.Source .Session.ID}}<a class="button button-small" href="{{.URL}}">{{.Label}}</a>{{end}}
            <span class="copy-control">
              <button type="button" class="button-secondary button-small" data-copy-text="{{.Session.Path}}">Copy JSONL Path</button>
              <span class="copy-feedback" role="status" aria-live="polite"></span>
            </span>
          </div>
        </div>
        <div class="meta">
          ` + sourceBadgeHTML + `
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
          <h2><a href="/sessions/{{.Source}}/{{.ID}}">{{if .Title}}{{.Title}}{{else}}{{.ID}}{{end}}</a></h2>
          <div class="card-actions">
            {{with sessionAppLink .Source .ID}}<a class="button button-small" href="{{.URL}}">{{.Label}}</a>{{end}}
            <span class="copy-control">
              <button type="button" class="button-secondary button-small" data-copy-text="{{.Path}}">Copy JSONL Path</button>
              <span class="copy-feedback" role="status" aria-live="polite"></span>
            </span>
          </div>
        </div>
        <div class="meta">
          ` + sourceBadgeHTML + `
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
        {{with .Session}}` + sourceBadgeHTML + `{{end}}
        {{if .Session.Archived}}<span class="badge archived">archived</span>{{end}}
        {{if .Session.StartedAtMS.Valid}}<span>started {{formatTime .Session.StartedAtMS}}</span>{{end}}
        {{if .Session.UpdatedAtMS.Valid}}<span>updated {{formatTime .Session.UpdatedAtMS}}</span>{{end}}
        <span>{{formatSize .Session.Size}}</span>
      </div>
      <div class="subtle" style="margin-top:8px">{{.Session.ID}}{{if .Session.CWD}} · {{.Session.CWD}}{{end}}</div>
    </div>
    <div class="button-row">
      {{with sessionAppLink .Session.Source .Session.ID}}<a class="button" href="{{.URL}}">{{.Label}}</a>{{end}}
      <span class="copy-control">
        <button type="button" class="button-secondary" data-copy-text="{{.Session.Path}}">Copy JSONL Path</button>
        <span class="copy-feedback" role="status" aria-live="polite"></span>
      </span>
    </div>
  </header>

  <form class="search" action="/sessions/{{.Session.Source}}/{{.Session.ID}}" method="get" data-live-search>
    <div class="query-field">
      <input name="q" value="{{.Query}}" placeholder="Filter this session" autocomplete="off">
      <button type="button" class="query-clear" data-clear-query aria-label="Clear filter"{{if not .Query}} hidden{{end}}>×</button>
    </div>
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
  const copyTimers = new WeakMap();

  function showCopyFeedback(button, message) {
    const feedback = button.parentElement?.querySelector(".copy-feedback");
    if (!feedback) return;

    const previousTimer = copyTimers.get(feedback);
    if (previousTimer) window.clearTimeout(previousTimer);
    feedback.textContent = message;
    feedback.classList.add("copy-feedback-visible");
    copyTimers.set(feedback, window.setTimeout(() => {
      feedback.classList.remove("copy-feedback-visible");
      copyTimers.delete(feedback);
    }, 1500));
  }

  async function copyText(button) {
    const text = button.dataset.copyText;
    if (!text) return;

    try {
      await navigator.clipboard.writeText(text);
      showCopyFeedback(button, "Copied");
    } catch (_) {
      showCopyFeedback(button, "Copy failed");
    }
  }

  document.addEventListener("click", (event) => {
    const button = event.target.closest("[data-copy-text]");
    if (button) void copyText(button);
  });

  const form = document.querySelector("form[data-live-search]");
  const input = form?.querySelector('input[name="q"]');
  const clearButton = form?.querySelector("[data-clear-query]");
  const status = document.querySelector(".live-status");
  let results = document.querySelector("#results");
  let history = document.querySelector("#search-history");
  if (!form || !input || !status || !results) return;

  let timer;
  let controller;
  let composing = false;

  function updateClearButton() {
    if (clearButton) clearButton.hidden = input.value === "";
  }

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
    updateClearButton();
    scheduleSearch();
  });
  input.addEventListener("input", () => {
    updateClearButton();
    scheduleSearch();
  });
  input.addEventListener("keydown", (event) => {
    if (event.key === "Enter" && !event.isComposing && !composing) {
      event.preventDefault();
      cancelPending();
      form.requestSubmit();
    }
  });
  clearButton?.addEventListener("click", () => {
    input.value = "";
    updateClearButton();
    input.focus();
    void runSearch();
  });
  form.addEventListener("submit", cancelPending);
  updateClearButton();
})();`
