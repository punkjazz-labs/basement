package docredact

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// minBirthAge and maxBirthAge bound what counts as a plausible birth year,
// relative to today. A "date" outside this window (a delivery date, a
// contract date, a future date) is not flagged as a DOB -- this is the
// detector's whole reason for existing separately from a generic date
// finder, per the spec: "only flagged as DOB when plausible birth-year
// range."
const (
	minBirthAge = 0
	maxBirthAge = 120
)

func plausibleBirthYear(year int) bool {
	now := time.Now().Year()
	return year >= now-maxBirthAge && year <= now-minBirthAge
}

// numericDatePattern matches D/M/Y or M/D/Y written with "/" or "-":
// two 1-2 digit groups then a 4-digit year. Which of the two numeric
// groups is the day and which is the month is genuinely ambiguous without
// knowing the document's locale, so validNumericDate tries both readings
// and accepts the date if either is a real calendar date in range.
var numericDatePattern = regexp.MustCompile(`\b(\d{1,2})([/\-])(\d{1,2})[/\-](\d{4})\b`)

// isoDatePattern matches YYYY-MM-DD, unambiguous by construction.
var isoDatePattern = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)

// dottedDatePattern matches D.M.YYYY or DD.MM.YYYY, the German/European
// dotted numeric form, with the same either-reading-could-be-day-or-month
// ambiguity numericDatePattern resolves for the slash/dash form: Detect
// tries both orderings and accepts whichever is a real calendar date.
var dottedDatePattern = regexp.MustCompile(`\b(\d{1,2})\.(\d{1,2})\.(\d{4})\b`)

// cjkDatePattern matches YYYY年M月D日, used in both zh and ja documents.
// \b is ASCII-word-boundary based and doesn't do anything useful next to
// 年/月/日 (they're outside \b's \w class regardless), so this pattern
// doesn't lean on \b there at all -- the ideographs themselves are exact,
// unambiguous anchors, stronger than a boundary check would be. The
// leading \b is still meaningful: it's checked between two ASCII digits
// (a would-be preceding digit and the year's first digit), and it's what
// keeps a match from starting mid-way through a longer digit run.
var cjkDatePattern = regexp.MustCompile(`\b(\d{4})年(\d{1,2})月(\d{1,2})日`)

// monthNames is every literal month-name spelling the written-date
// patterns below recognize, across every language this detector
// currently supports: English, Italian, French, German, Spanish,
// Portuguese, Dutch, and Russian (nominative and genitive -- Russian
// dates are written in the genitive, "12 марта", so that's the form that
// actually needs to match, but the nominative is included too since it's
// the form most likely to appear as a bare month reference). Some
// spellings coincide across languages (Italian and Spanish both write
// "marzo"); that's harmless duplication since both resolve to the same
// month number below.
var monthNames = []string{
	// English
	"January", "February", "March", "April", "May", "June",
	"July", "August", "September", "October", "November", "December",
	// Italian
	"gennaio", "febbraio", "marzo", "aprile", "maggio", "giugno",
	"luglio", "agosto", "settembre", "ottobre", "novembre", "dicembre",
	// French
	"janvier", "février", "mars", "avril", "mai", "juin",
	"juillet", "août", "septembre", "octobre", "novembre", "décembre",
	// German
	"Januar", "Februar", "März", "April", "Mai", "Juni",
	"Juli", "August", "September", "Oktober", "November", "Dezember",
	// Spanish
	"enero", "febrero", "marzo", "abril", "mayo", "junio",
	"julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre",
	// Portuguese
	"janeiro", "fevereiro", "março", "abril", "maio", "junho",
	"julho", "agosto", "setembro", "outubro", "novembro", "dezembro",
	// Dutch
	"januari", "februari", "maart", "april", "mei", "juni",
	"juli", "augustus", "september", "oktober", "november", "december",
	// Russian, nominative
	"январь", "февраль", "март", "апрель", "май", "июнь",
	"июль", "август", "сентябрь", "октябрь", "ноябрь", "декабрь",
	// Russian, genitive (the form used in dates, e.g. "12 марта")
	"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря",
}

var monthByName = func() map[string]int {
	m := make(map[string]int, len(monthNames))
	for i, name := range monthNames {
		m[strings.ToLower(name)] = i%12 + 1
	}
	return m
}()

// monthWord is "January|February|...|декабря", used in both written-month
// patterns below.
var monthWord = strings.Join(monthNames, "|")

// writtenDayMonthYear matches "12 March 1990", "12th March, 1990", the
// German "12. März 1990" (a dot after the day, not just after the month),
// and the Spanish/Portuguese connective form "12 de marzo de 1990" / "12
// de março de 1990" (both "de"s optional so the plain non-connective
// languages keep matching unchanged).
var writtenDayMonthYear = regexp.MustCompile(
	`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\.?\s+(?:de\s+)?(` + monthWord + `)\.?,?\s+(?:de\s+)?(\d{4})\b`,
)

// writtenMonthDayYear matches "March 12, 1990" or "March 12 1990".
var writtenMonthDayYear = regexp.MustCompile(
	`(?i)\b(` + monthWord + `)\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b`,
)

type DOBDetector struct{}

func (DOBDetector) Name() string { return "dob" }

func (DOBDetector) Detect(text string) []Match {
	var out []Match

	// FindAllStringSubmatchIndex returns, per match, a flat slice of
	// (start, end) int pairs: indices 0,1 are the whole match, and each
	// subsequent pair loc[2*N],loc[2*N+1] is the start/end of capture
	// group N -- text[loc[2*N]:loc[2*N+1]] regardless of that group's
	// matched length.
	for _, loc := range numericDatePattern.FindAllStringSubmatchIndex(text, -1) {
		a, _ := strconv.Atoi(text[loc[2]:loc[3]])
		b, _ := strconv.Atoi(text[loc[6]:loc[7]])
		year, _ := strconv.Atoi(text[loc[8]:loc[9]])
		if isCalendarDate(a, b, year) || isCalendarDate(b, a, year) {
			if plausibleBirthYear(year) {
				out = append(out, newDateMatch(text, loc[0], loc[1]))
			}
		}
	}

	for _, loc := range isoDatePattern.FindAllStringSubmatchIndex(text, -1) {
		year, _ := strconv.Atoi(text[loc[2]:loc[3]])
		month, _ := strconv.Atoi(text[loc[4]:loc[5]])
		day, _ := strconv.Atoi(text[loc[6]:loc[7]])
		if isCalendarDate(day, month, year) && plausibleBirthYear(year) {
			out = append(out, newDateMatch(text, loc[0], loc[1]))
		}
	}

	for _, loc := range dottedDatePattern.FindAllStringSubmatchIndex(text, -1) {
		// \b sits happily between the trailing year digit and a
		// following ".", so on its own it doesn't stop this pattern
		// from matching just the "1.2.2020" slice of the longer,
		// non-date run "1.2.2020.3". Only treat a following "." as
		// disqualifying when another digit follows it -- that's the
		// shape of more dotted-number noise, not of a date followed
		// by an ordinary sentence-final period.
		if loc[1] < len(text) && text[loc[1]] == '.' &&
			loc[1]+1 < len(text) && isASCIIDigit(text[loc[1]+1]) {
			continue
		}
		if loc[0] >= 2 && text[loc[0]-1] == '.' && isASCIIDigit(text[loc[0]-2]) {
			continue
		}
		day, _ := strconv.Atoi(text[loc[2]:loc[3]])
		month, _ := strconv.Atoi(text[loc[4]:loc[5]])
		year, _ := strconv.Atoi(text[loc[6]:loc[7]])
		if isCalendarDate(day, month, year) || isCalendarDate(month, day, year) {
			if plausibleBirthYear(year) {
				out = append(out, newDateMatch(text, loc[0], loc[1]))
			}
		}
	}

	for _, loc := range cjkDatePattern.FindAllStringSubmatchIndex(text, -1) {
		year, _ := strconv.Atoi(text[loc[2]:loc[3]])
		month, _ := strconv.Atoi(text[loc[4]:loc[5]])
		day, _ := strconv.Atoi(text[loc[6]:loc[7]])
		if isCalendarDate(day, month, year) && plausibleBirthYear(year) {
			out = append(out, newDateMatch(text, loc[0], loc[1]))
		}
	}

	for _, loc := range writtenDayMonthYear.FindAllStringSubmatchIndex(text, -1) {
		day, _ := strconv.Atoi(text[loc[2]:loc[3]])
		month := monthByName[strings.ToLower(text[loc[4]:loc[5]])]
		year, _ := strconv.Atoi(text[loc[6]:loc[7]])
		if isCalendarDate(day, month, year) && plausibleBirthYear(year) {
			out = append(out, newDateMatch(text, loc[0], loc[1]))
		}
	}

	for _, loc := range writtenMonthDayYear.FindAllStringSubmatchIndex(text, -1) {
		month := monthByName[strings.ToLower(text[loc[2]:loc[3]])]
		day, _ := strconv.Atoi(text[loc[4]:loc[5]])
		year, _ := strconv.Atoi(text[loc[6]:loc[7]])
		if isCalendarDate(day, month, year) && plausibleBirthYear(year) {
			out = append(out, newDateMatch(text, loc[0], loc[1]))
		}
	}

	return out
}

func newDateMatch(text string, start, end int) Match {
	return Match{Start: start, End: end, Text: text[start:end], Category: CategoryDOB, Source: Source}
}

// isASCIIDigit reports whether b is an ASCII '0'-'9' digit -- used by
// Detect's dotted-date guard, which inspects a single byte just outside a
// match and needs a plain digit test, not a full number parse.
func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// isCalendarDate reports whether day/month/year is a real date -- it
// rejects things like day 31 in April by round-tripping through time.Date
// and checking the components survived, since time.Date silently
// normalizes out-of-range values instead of erroring.
func isCalendarDate(day, month, year int) bool {
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return false
	}
	t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	return t.Year() == year && int(t.Month()) == month && t.Day() == day
}
