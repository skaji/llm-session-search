package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	apiSearchDefaultLimit   = 20
	apiSearchMatchesPerItem = 3
	apiSearchSnippetRunes   = 240
)

type apiSearchResponse struct {
	Query      string            `json:"query"`
	Results    []apiSearchResult `json:"results"`
	HasMore    bool              `json:"has_more"`
	NextOffset *int              `json:"next_offset"`
}

type apiSearchResult struct {
	Source     string           `json:"source"`
	SessionID  string           `json:"session_id"`
	Path       string           `json:"path"`
	URL        *string          `json:"url"`
	Title      string           `json:"title"`
	CWD        string           `json:"cwd"`
	Archived   bool             `json:"archived"`
	StartedAt  *string          `json:"started_at"`
	UpdatedAt  *string          `json:"updated_at"`
	MatchCount int              `json:"match_count"`
	Matches    []apiSearchMatch `json:"matches"`
}

type apiSearchMatch struct {
	LineNumber int     `json:"line_number"`
	Timestamp  *string `json:"timestamp"`
	Role       string  `json:"role"`
	Phase      string  `json:"phase"`
	Snippet    string  `json:"snippet"`
}

func (app *webApp) apiSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, err := apiIntegerParameter(r, "limit", apiSearchDefaultLimit, 1)
	if err != nil {
		writeAPIJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	offset, err := apiIntegerParameter(r, "offset", 0, 0)
	if err != nil {
		writeAPIJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	hits, err := app.store.Search(r.Context(), query, limit+1, offset)
	if err != nil {
		writeAPIJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	hasMore := len(hits) > limit
	if hasMore {
		hits = hits[:limit]
	}
	results := make([]apiSearchResult, 0, len(hits))
	terms := parseSearchQuery(query)
	for _, hit := range hits {
		records, err := app.store.sessionMatchRecords(r.Context(), hit.Key, query, apiSearchMatchesPerItem)
		if err != nil {
			writeAPIJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		matches := make([]apiSearchMatch, 0, len(records))
		for _, record := range records {
			matches = append(matches, apiSearchMatch{
				LineNumber: record.LineNumber,
				Timestamp:  apiTimestamp(record.TimestampMS),
				Role:       record.Role,
				Phase:      record.Phase,
				Snippet:    makeSnippet(record.Text, terms, apiSearchSnippetRunes),
			})
		}
		results = append(results, apiSearchResult{
			Source:     hit.Source,
			SessionID:  hit.ID,
			Path:       hit.Path,
			URL:        codexSessionURL(hit.Source, hit.ID),
			Title:      hit.Title,
			CWD:        hit.CWD,
			Archived:   hit.Archived,
			StartedAt:  apiTimestamp(hit.StartedAtMS),
			UpdatedAt:  apiTimestamp(hit.UpdatedAtMS),
			MatchCount: hit.MatchCount,
			Matches:    matches,
		})
	}

	var nextOffset *int
	if hasMore {
		next := offset + limit
		nextOffset = &next
	}
	writeAPIJSON(w, http.StatusOK, apiSearchResponse{
		Query:      query,
		Results:    results,
		HasMore:    hasMore,
		NextOffset: nextOffset,
	})
}

func apiIntegerParameter(r *http.Request, name string, defaultValue, minimum int) (int, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum {
		return 0, fmt.Errorf("%s must be an integer greater than or equal to %d", name, minimum)
	}
	return parsed, nil
}

func apiTimestamp(value sql.NullInt64) *string {
	if !value.Valid {
		return nil
	}
	formatted := time.UnixMilli(value.Int64).UTC().Format(time.RFC3339Nano)
	return &formatted
}

func codexSessionURL(source, id string) *string {
	if source != sourceCodex || !uuidPattern.MatchString(id) {
		return nil
	}
	value := "codex://threads/" + id
	return &value
}

func writeAPIJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
