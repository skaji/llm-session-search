package main

import (
	"reflect"
	"testing"
)

func TestParseSearchQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		query string
		want  []string
	}{
		{query: "tinyenv github", want: []string{"tinyenv", "github"}},
		{query: `tinyenv "github actions" Go`, want: []string{"tinyenv", "github actions", "Go"}},
		{query: `"unfinished phrase`, want: []string{"unfinished phrase"}},
		{query: `   `},
	}
	for _, test := range tests {
		test := test
		t.Run(test.query, func(t *testing.T) {
			t.Parallel()
			if got := parseSearchQuery(test.query); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseSearchQuery(%q) = %#v, want %#v", test.query, got, test.want)
			}
		})
	}
}

func TestHighlightText(t *testing.T) {
	t.Parallel()
	parts := highlightText("TinyEnv and GITHUB", []string{"tinyenv", "github"})
	want := []textPart{
		{Text: "TinyEnv", Match: true},
		{Text: " and ", Match: false},
		{Text: "GITHUB", Match: true},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("highlightText() = %#v, want %#v", parts, want)
	}
}
