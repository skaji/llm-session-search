package main

import (
	"slices"
	"strings"
	"unicode"
)

type textPart struct {
	Text  string
	Match bool
}

func parseSearchQuery(query string) []string {
	var (
		terms   []string
		current []rune
		quoted  bool
	)
	flush := func() {
		text := strings.TrimSpace(string(current))
		if text != "" {
			terms = append(terms, text)
		}
		current = current[:0]
	}

	for _, char := range query {
		switch {
		case char == '"':
			flush()
			quoted = !quoted
		case unicode.IsSpace(char) && !quoted:
			flush()
		default:
			current = append(current, char)
		}
	}
	flush()
	return terms
}

func highlightText(text string, terms []string) []textPart {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	matched := matchedRunes(runes, terms)

	parts := make([]textPart, 0, 3)
	start := 0
	for index := 1; index <= len(runes); index++ {
		if index == len(runes) || matched[index] != matched[start] {
			parts = append(parts, textPart{Text: string(runes[start:index]), Match: matched[start]})
			start = index
		}
	}
	return parts
}

func firstMatchRune(text string, terms []string) int {
	lowerText := lowerRunes([]rune(text))
	first := -1
	for _, term := range terms {
		needle := lowerRunes([]rune(term))
		for index := 0; len(needle) > 0 && index+len(needle) <= len(lowerText); index++ {
			if slices.Equal(lowerText[index:index+len(needle)], needle) {
				if first < 0 || index < first {
					first = index
				}
				break
			}
		}
	}
	return first
}

func matchedRunes(text []rune, terms []string) []bool {
	lowerText := lowerRunes(text)
	matched := make([]bool, len(text))
	for _, term := range terms {
		needle := lowerRunes([]rune(term))
		for start := 0; len(needle) > 0 && start+len(needle) <= len(lowerText); start++ {
			if slices.Equal(lowerText[start:start+len(needle)], needle) {
				for index := start; index < start+len(needle); index++ {
					matched[index] = true
				}
			}
		}
	}
	return matched
}

func lowerRunes(runes []rune) []rune {
	lower := make([]rune, len(runes))
	for index, char := range runes {
		lower[index] = unicode.ToLower(char)
	}
	return lower
}
