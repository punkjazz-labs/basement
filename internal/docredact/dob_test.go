package docredact

import (
	"strings"
	"testing"
)

// dobPositiveCases pairs a sentence-context document with the exact DOB
// literal it must find within it -- one per newly added format family:
// the dotted D.M.YYYY numeric form, the it/fr/de/es/pt/nl/ru written-month
// forms (Russian in the genitive, the form dates are actually written in),
// and the CJK YYYY年M月D日 form shared by zh and ja documents.
var dobPositiveCases = []struct {
	name string
	text string
	want string
}{
	{
		name: "dotted numeric",
		text: "Geburtsdatum: 12.03.1990, laut Akte.",
		want: "12.03.1990",
	},
	{
		name: "italian written",
		text: "È nata il 12 marzo 1990 a Roma.",
		want: "12 marzo 1990",
	},
	{
		name: "french written",
		text: "Elle est née le 12 mars 1990 à Paris.",
		want: "12 mars 1990",
	},
	{
		name: "german written, dot after day",
		text: "Sie wurde am 12. März 1990 geboren.",
		want: "12. März 1990",
	},
	{
		name: "spanish written, de connective",
		text: "Nació el 12 de marzo de 1990 en Madrid.",
		want: "12 de marzo de 1990",
	},
	{
		name: "portuguese written, de connective",
		text: "Ela nasceu em 12 de março de 1990 em Lisboa.",
		want: "12 de março de 1990",
	},
	{
		name: "dutch written",
		text: "Zij is geboren op 12 maart 1990 in Amsterdam.",
		want: "12 maart 1990",
	},
	{
		name: "russian written, genitive",
		text: "Она родилась 12 марта 1990 года в Москве.",
		want: "12 марта 1990",
	},
	{
		name: "cjk year-month-day",
		text: "生年月日は1990年3月12日です。",
		want: "1990年3月12日",
	},
}

func TestDOBDetectorNewFormats(t *testing.T) {
	for _, c := range dobPositiveCases {
		t.Run(c.name, func(t *testing.T) {
			matches := DOBDetector{}.Detect(c.text)
			if len(matches) != 1 {
				t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
			}
			if matches[0].Text != c.want {
				t.Errorf("Text = %q, want %q", matches[0].Text, c.want)
			}
			if matches[0].Category != CategoryDOB {
				t.Errorf("Category = %q, want %q", matches[0].Category, CategoryDOB)
			}
			wantStart := strings.Index(c.text, c.want)
			if matches[0].Start != wantStart || matches[0].End != wantStart+len(c.want) {
				t.Errorf("span = [%d,%d), want [%d,%d)", matches[0].Start, matches[0].End, wantStart, wantStart+len(c.want))
			}
		})
	}
}

// TestDOBDetectorRussianCaseInsensitive proves (?i) actually folds case
// for Cyrillic in Go's regexp -- it does for Unicode letters, but that's
// worth pinning down explicitly rather than assuming, per both the
// nominative and genitive spellings the month table carries.
func TestDOBDetectorRussianCaseInsensitive(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"lowercase genitive", "Дата рождения: 12 марта 1990 года.", "12 марта 1990"},
		{"capitalized genitive", "Дата рождения: 12 Марта 1990 года.", "12 Марта 1990"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			matches := DOBDetector{}.Detect(c.text)
			if len(matches) != 1 {
				t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
			}
			if matches[0].Text != c.want {
				t.Errorf("Text = %q, want %q", matches[0].Text, c.want)
			}
		})
	}
}

// dobNegativeCases are inputs that look like one of the new formats on the
// surface but must never be flagged: a version string and a longer dotted-
// digit run that only happen to contain a valid-shaped date as a slice of
// themselves, a dotted date whose year is outside the plausible birth-year
// window, a real CJK calendar date whose year is likewise out of window
// (1875, not the "born this year" edge case a low year like the current
// year would be), and an item-number-then-period sentence fragment that
// happens to be followed later by "Month YYYY" -- a regression caught in
// review: the day-dot allowance was briefly shared across every language
// in writtenDayMonthYear (instead of gated to German only), which made
// "12." (an item number plus a sentence-ending period) combine with an
// unrelated later "March 2020" into a false-positive match.
var dobNegativeCases = []struct {
	name string
	text string
}{
	{"version string", "Installed version 1.2.3 of the client."},
	{"dotted-run noise", "Reference number 1.2.2020.3 was issued."},
	{"dotted date out of birth window", "Record dated 12.03.1875 in the ledger."},
	{"cjk date out of birth window", "生年月日は1875年3月12日です。"},
	{"item number plus period, not a German date", "See item 12. March 2020 pricing applies to everyone."},
}

func TestDOBDetectorNewFormatsNegatives(t *testing.T) {
	for _, c := range dobNegativeCases {
		t.Run(c.name, func(t *testing.T) {
			matches := DOBDetector{}.Detect(c.text)
			if len(matches) != 0 {
				t.Errorf("got %d matches, want 0: %+v", len(matches), matches)
			}
		})
	}
}

// TestDOBDetectorDottedDateSentenceFinalPeriod guards the flip side of the
// dotted-run-noise negative above: an ordinary sentence-final period right
// after a dotted date must not be swallowed by the same guard, since a
// period followed by a non-digit (here, end of string) isn't the shape of
// dotted-number noise.
func TestDOBDetectorDottedDateSentenceFinalPeriod(t *testing.T) {
	text := "Geboren am 12.03.1990."
	matches := DOBDetector{}.Detect(text)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(matches), matches)
	}
	if matches[0].Text != "12.03.1990" {
		t.Errorf("Text = %q, want %q", matches[0].Text, "12.03.1990")
	}
}
