package docredact

import (
	"encoding/json"
	"errors"
	"strings"
)

// ErrNoModelItems: the reply had no array json.Unmarshal or the bracket
// walker below could make sense of, so there is nothing to extract -- not
// the same fact as "the model found nothing to redact", which is a reply
// that does parse into an empty array.
var ErrNoModelItems = errors.New("model reply has no parseable item array")

// ModelItem is one literal the model pass named in its reply, before its
// Category has been checked against ParseCategory -- that check is
// ApplyModelPass's job, not this parser's.
type ModelItem struct {
	Literal  string `json:"literal"`
	Category string `json:"category"`
}

// ExtractModelItems parses a model's reply into the items it named. Models
// answer with a clean JSON array far less often than the prompt asks for
// one: prose before or after it, a markdown code fence around it, or the
// whole thing wrapped in an object are all routine, so this tries the whole
// trimmed reply first and, failing that, hunts for the first top-level
// array and parses that instead. A reply with no array anywhere is an
// error, not an empty result, so a caller can tell "found nothing" from
// "could not read the reply".
func ExtractModelItems(reply string) ([]ModelItem, error) {
	trimmed := strings.TrimSpace(reply)

	var items []ModelItem
	if err := json.Unmarshal([]byte(trimmed), &items); err == nil {
		return dropEmptyLiterals(items), nil
	}

	if candidate, ok := bracketedArray(trimmed); ok {
		if err := json.Unmarshal([]byte(candidate), &items); err == nil {
			return dropEmptyLiterals(items), nil
		}
	}

	return nil, ErrNoModelItems
}

// dropEmptyLiterals removes items whose literal is empty or whitespace
// only: a model that names one but leaves the text blank has not actually
// pointed at anything in the document.
func dropEmptyLiterals(items []ModelItem) []ModelItem {
	var kept []ModelItem
	for _, item := range items {
		if strings.TrimSpace(item.Literal) == "" {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// bracketedArray returns the substring of s from its first '[' to the ']'
// that closes it, tracking whether the walk is inside a JSON string (and
// whether the next character there is escaped) so a literal like "pay [in
// full]" cannot fool it into closing the array early. This is also what
// makes an object wrapper like {"items":[...]} work without special-casing
// it: the first '[' found is the one inside "items", and everything after
// the matching ']' -- including the wrapper's own closing brace -- is
// simply not part of the returned substring.
func bracketedArray(s string) (string, bool) {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return "", false
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}
