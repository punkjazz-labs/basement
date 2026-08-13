package docredact

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// cnIDPattern matches an 18-character Chinese resident ID: a 6-digit area
// code (first digit 1-9, the GB/T 2260 administrative-division code), an
// 8-digit birth date YYYYMMDD, a 3-digit sequence order, and a single
// check character (a digit or the letter X, upper or lower case -- the
// pattern accepts both so the exact-literal check in Detect can decide).
// Written contiguously with no conventional separator. cnIDBirthDateOK and
// cnIDCheckChar are the real validators.
var cnIDPattern = regexp.MustCompile(`\b([1-9]\d{5})(\d{4})(\d{2})(\d{2})(\d{3})([0-9Xx])\b`)

type CNResidentIDDetector struct{}

func (CNResidentIDDetector) Name() string { return "cn_resident_id" }

func (CNResidentIDDetector) Detect(text string) []Match {
	var out []Match
	for _, loc := range cnIDPattern.FindAllStringSubmatchIndex(text, -1) {
		area := text[loc[2]:loc[3]]
		year := text[loc[4]:loc[5]]
		month := text[loc[6]:loc[7]]
		day := text[loc[8]:loc[9]]
		order := text[loc[10]:loc[11]]
		check := text[loc[12]:loc[13]]

		yearNum, _ := strconv.Atoi(year)
		monthNum, _ := strconv.Atoi(month)
		dayNum, _ := strconv.Atoi(day)
		if !cnIDBirthDateOK(dayNum, monthNum, yearNum) {
			continue
		}

		first17 := area + year + month + day + order
		if strings.ToUpper(check) != cnIDCheckChar(first17) {
			continue
		}
		out = append(out, Match{
			Start:    loc[0],
			End:      loc[1],
			Text:     text[loc[0]:loc[1]],
			Category: CategoryCNResidentID,
			Source:   Source,
		})
	}
	return out
}

// cnIDBirthDateOK reports whether day/month/year is both a real calendar
// date (isCalendarDate, shared with dob.go) and within the range a GB
// 11643 birth date can plausibly fall in: 1900 through the current year.
func cnIDBirthDateOK(day, month, year int) bool {
	if year < 1900 || year > time.Now().Year() {
		return false
	}
	return isCalendarDate(day, month, year)
}

// cnIDCheckWeights is the published ISO 7064 mod 11-2 weight table for the
// first 17 digits of a Chinese resident ID: w[i] = 2^(17-i) mod 11 for
// i = 0..16, position i counted from the area code's first digit.
var cnIDCheckWeights = [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}

// cnIDCheckChars is GB 11643's published mapping from mod-11 remainder to
// check character, indexed by the remainder: "10X98765432".
const cnIDCheckChars = "10X98765432"

// cnIDCheckChar computes the expected 18th character (always uppercase,
// matching cnIDCheckChars) from the first 17 digits: sum each digit times
// its cnIDCheckWeights weight, reduce mod 11, and index into
// cnIDCheckChars.
func cnIDCheckChar(first17 string) string {
	sum := 0
	for i := 0; i < 17; i++ {
		sum += int(first17[i]-'0') * cnIDCheckWeights[i]
	}
	return string(cnIDCheckChars[sum%11])
}
