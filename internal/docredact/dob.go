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

var monthNames = []string{
	"January", "February", "March", "April", "May", "June",
	"July", "August", "September", "October", "November", "December",
}

var monthByName = func() map[string]int {
	m := make(map[string]int, len(monthNames))
	for i, name := range monthNames {
		m[strings.ToLower(name)] = i + 1
	}
	return m
}()

// monthWord is "January|February|...|December", used in both written-month
// patterns below.
var monthWord = strings.Join(monthNames, "|")

// writtenDayMonthYear matches "12 March 1990" or "12th March, 1990".
var writtenDayMonthYear = regexp.MustCompile(
	`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+(` + monthWord + `)\.?,?\s+(\d{4})\b`,
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
