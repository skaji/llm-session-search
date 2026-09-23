# llm-session-search

`llm-session-search` indexes local Codex and Claude conversations and makes
them searchable from a web browser, scripts, and coding agents.

![LLM Session Search screenshot](maint/screenshot.png)

## Quick start

Download the appropriate binary for your platform from
[GitHub Releases](https://github.com/skaji/llm-session-search/releases), extract
it, and place `llm-session-search` somewhere in your `PATH`.

Start the local server in the background:

```console
llm-session-search --daemon
```

Then open <http://127.0.0.1:8787/>.

## Agent integration

The `session-history` skill lets coding agents search previous conversations
through the local HTTP API. Install and start `llm-session-search` before using
the skill. The skill does not start, stop, or restart the server.

### Codex App and Codex CLI

Ask Codex to install the skill:

```text
$skill-installer Install the session-history skill from https://github.com/skaji/llm-session-search/tree/main/skills/session-history
```

### Claude Code App and Claude Code CLI

Clone this repository, then link the skill into your personal Claude Code
skills directory:

```console
mkdir -p ~/.claude/skills
ln -s /path/to/llm-session-search/skills/session-history \
  ~/.claude/skills/session-history
```

## Search

Use the web interface at <http://127.0.0.1:8787/> or call the read-only JSON
API:

```console
curl --get \
  --data-urlencode 'q=tinyenv "github actions"' \
  --data-urlencode 'cwd=/path/to/project' \
  --data-urlencode 'limit=10' \
  http://127.0.0.1:8787/api/v1/search
```

Space-separated terms use session-level AND semantics, so terms may occur in
different messages. Double quotes match a phrase. The optional `cwd` parameter
limits results to that working directory and its descendants. Use `offset` with
the returned `next_offset` to fetch another page.

Results are grouped by session and include the source JSONL path, session ID,
title, working directory, timestamps, and matching records. Search terms are
highlighted in the web interface, and sessions can be opened in their
respective desktop app.

## How it works

On startup, `llm-session-search` creates or updates
`~/.llm-session-search/index.db` from:

- `~/.codex/sessions/**/*.jsonl`
- `~/.codex/archived_sessions/**/*.jsonl`
- `${CLAUDE_CONFIG_DIR:-~/.claude}/projects/*/*.jsonl`

The index is updated every minute. Unchanged files are skipped, append-only
updates resume from the previous offset, and source files are never modified.
Claude subagent transcripts are not indexed.

Only user and assistant messages are searchable. System, developer, tool,
thinking, and other internal records are excluded, as are encrypted content,
base64 data URLs, automatically injected context, Claude system reminders, and
local commands.

The index database has no schema migration support. After upgrading to a
version that changes the schema or indexed content, stop the server, delete
`~/.llm-session-search/index.db`, and start it again.

## Server management

```console
llm-session-search --daemon-status
llm-session-search --daemon-stop
llm-session-search --daemon-restart
```

Common options:

```console
llm-session-search \
  --codex-home /path/to/.codex \
  --claude-home /path/to/.claude \
  --data-dir /path/to/data \
  --listen 127.0.0.1:9000 \
  --index-interval 1m
```

Set `--index-interval 0` to disable periodic updates. The startup update always
runs. When using a custom `--data-dir`, pass the same option to later daemon
status and control commands.

## Privacy

The server listens on `127.0.0.1` by default and uses no external assets. The
data directory is created with mode `0700`, and the SQLite database uses mode
`0600`. The database contains extracted text from local Codex and Claude
sessions and should be treated as sensitive.

## Development

Build and test the project with Go, then run the repository lint checks:

```console
go build ./...
go test ./...
bash maint/lint.sh
```
