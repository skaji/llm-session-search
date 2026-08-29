# llm-session-search

`llm-session-search` indexes user and assistant messages from local Codex JSONL
sessions and provides a web interface for finding previous conversations. Its
best-effort parser supports multiple session schema generations.

## Build and run

```console
go build
./llm-session-search
```

Open <http://127.0.0.1:8787/>. Before listening, the application creates or
updates `~/.llm-session-search/index.db` from:

- `~/.codex/sessions/**/*.jsonl`
- `~/.codex/archived_sessions/**/*.jsonl`

It then updates the index every minute. Unchanged files are skipped, append-only
updates resume from the previous offset, and source files are never modified.

Available options:

```console
./llm-session-search \
  --codex-home /path/to/.codex \
  --db /path/to/index.db \
  --listen 127.0.0.1:9000 \
  --index-interval 1m
```

Set `--index-interval 0` to disable background updates. The startup update
always runs.

## Search behavior

Space-separated terms use session-level AND semantics, so terms may occur in
different messages. Double quotes match a phrase, for example
`tinyenv "github actions"`. Terms of at least three Unicode characters use
SQLite FTS5 trigram search; shorter terms use a substring scan.

Typing triggers a search after 500 milliseconds when all terms are at least
three characters. Whitespace, a closing quote, Enter, or the Search button runs
it immediately. Search terms are highlighted in result and session pages.

The left pane stores the 50 most recent distinct global queries. Session pages
show role and phase badges, provide a `codex://threads/<session-id>` link, and
can copy the current JSONL path. System, developer, tool, and other internal
records are neither indexed nor displayed. Encrypted content, base64 data URLs,
and automatically injected AGENTS.md, plugin, and environment context are also
excluded.

## Privacy

The web server listens on `127.0.0.1` by default and uses no external assets.
The data directory is created with mode `0700`, and the SQLite database uses
mode `0600`. The database still contains extracted text from local sessions and
should be treated as sensitive.
