// Package match decides whether a job is one a subscriber wants.
//
// Keywords match whole words, not substrings: "intern" must not match
// "Internal Tools Engineer", and "java" must not match "JavaScript".
package match

import (
	"slices"
	"strings"
	"unicode"
)

// Filter is a subscriber's wishes, as typed. Empty Include or Locations means "anything".
type Filter struct {
	Include   []string // the title must contain at least one of these
	Exclude   []string // the title must contain none of these; wins over Include
	Locations []string // the job's location must contain at least one of these
}

// aliases folds spelling variants into one form. It is applied to job text and
// keywords alike, so either spelling on either side matches the other.
var aliases = map[string]string{
	"bangalore": "bengaluru",
	"blr":       "bengaluru",
	"gurgaon":   "gurugram",
	"bombay":    "mumbai",
	"sr":        "senior",
	"jr":        "junior",
	"i":         "1", // "SDE I" and "SDE 1" are the same level
	"ii":        "2",
	"iii":       "3",
	"iv":        "4",
}

// Matcher is a Filter with every keyword already split into words,
// so matching thousands of jobs doesn't re-split the same keywords each time.
type Matcher struct {
	include, exclude, locations [][]string
}

// Compile prepares f for matching.
func Compile(f Filter) Matcher {
	return Matcher{
		include:   phrases(f.Include),
		exclude:   phrases(f.Exclude),
		locations: phrases(f.Locations),
	}
}

// Match reports whether a job with this title and location passes the filter.
func (m Matcher) Match(title, location string) bool {
	t := words(title)
	if len(m.include) > 0 && !containsAny(t, m.include) {
		return false
	}
	if containsAny(t, m.exclude) {
		return false
	}
	if len(m.locations) > 0 && !containsAny(words(location), m.locations) {
		return false
	}
	return true
}

// words lowercases s, splits it into words and applies aliases.
// "+" and "#" count as part of a word so "C++" and "C#" survive.
// Found on real data: "Staff+ Software Engineer" slipped past an exclude of "staff".
func words(s string) []string {
	ws := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' && r != '#'
	})
	for i, w := range ws {
		// "C++" keeps its pluses, but "Staff+" means staff and above, so it must still match "staff".
		if t := strings.TrimRight(w, "+"); len(t) > 1 {
			w = t
		}
		if a, ok := aliases[w]; ok {
			w = a
		}
		ws[i] = w
	}
	return ws
}

// phrases splits each keyword into words, dropping keywords with no words at all.
func phrases(keywords []string) [][]string {
	var ps [][]string
	for _, k := range keywords {
		if w := words(k); len(w) > 0 {
			ps = append(ps, w)
		}
	}
	return ps
}

// containsAny reports whether any phrase appears in text as consecutive words.
func containsAny(text []string, phrases [][]string) bool {
	for _, p := range phrases {
		for i := 0; i+len(p) <= len(text); i++ {
			if slices.Equal(text[i:i+len(p)], p) {
				return true
			}
		}
	}
	return false
}
