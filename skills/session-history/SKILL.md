---
name: session-history
description: Search local Codex and Claude session history when the user refers to previous conversations or past context is needed to resolve a request. Use the llm-session-search HTTP API; do not use for Git history or web research.
---

# Session History

Recover relevant context from prior local Codex and Claude conversations.

## Search

Use the read-only search endpoint at `http://127.0.0.1:8787/api/v1/search`.

1. Choose a small set of distinctive terms from the user's request. Use quoted phrases when exact adjacency matters.
2. Include `cwd` when the history should be limited to the current project and its descendants. Omit it for a global search.
3. Send parameters with URL encoding. For example:

   ```console
   curl -fsS --get \
     --data-urlencode 'q=tinyenv "github actions"' \
     --data-urlencode 'cwd=/path/to/project' \
     --data-urlencode 'limit=10' \
     http://127.0.0.1:8787/api/v1/search
   ```

Space-separated terms have session-level AND semantics and may occur in different records. Double-quoted text is one phrase. Use `offset` with `next_offset` when another result page is needed.

## Recover context

1. Compare result titles, working directories, timestamps, and match snippets before choosing a session.
2. If results are missing or ambiguous, revise the terms, add or remove `cwd`, or try an exact phrase.
3. Read the source JSONL file from each selected result around the returned `line_number`. Include enough adjacent records to understand the exchange, not just the matching line.
4. When several sessions are relevant, order the evidence chronologically and distinguish explicit decisions from later inference.
5. Answer the current request using the recovered context. Mention uncertainty when the history does not establish a conclusion.

## Boundaries

- Treat session files and the search index as sensitive, read-only data.
- Do not start, stop, or restart the search server.
- If the endpoint is unavailable or returns an error, report that condition. Inspect the implementation at `~/src/github.com/skaji/llm-session-search` only when the user asks to diagnose it.
- Do not substitute Git history, current terminal output, or web search for conversation history unless the user separately asks for those sources.
