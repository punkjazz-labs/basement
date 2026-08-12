package docredact

import (
	"regexp"
	"strings"
)

// codiceFiscalePattern matches the fixed 16-character shape: 6 letters
// (surname+name consonants/vowels), 2 digits (year), 1 letter (month
// code), 2 digits (day, +40 for a female-coded record), 4 characters (1
// letter + 3 digits, the birthplace code), 1 check letter. cfCheckDigit is
// the real validator.
var codiceFiscalePattern = regexp.MustCompile(`\b[A-Za-z]{6}\d{2}[A-Za-z]\d{2}[A-Za-z]\d{3}[A-Za-z]\b`)

type ITCodiceFiscaleDetector struct{}

func (ITCodiceFiscaleDetector) Name() string { return "it_codice_fiscale" }

func (ITCodiceFiscaleDetector) Detect(text string) []Match {
	return matchesFromRegexp(text, codiceFiscalePattern, CategoryITCF, func(s string) bool {
		upper := strings.ToUpper(s)
		return cfCheckDigit(upper[:15]) == rune(upper[15])
	})
}

// cfOddTable and cfEvenTable are the fixed conversion tables the Agenzia
// delle Entrate algorithm uses for characters at odd (1st, 3rd, ...) and
// even (2nd, 4th, ...) 1-indexed positions of the first 15 characters.
// These are published constants, not derived from anything -- copied here
// verbatim, digits before letters in each table for readability.
var cfOddTable = map[byte]int{
	'0': 1, '1': 0, '2': 5, '3': 7, '4': 9, '5': 13, '6': 15, '7': 17, '8': 19, '9': 21,
	'A': 1, 'B': 0, 'C': 5, 'D': 7, 'E': 9, 'F': 13, 'G': 15, 'H': 17, 'I': 19, 'J': 21,
	'K': 2, 'L': 4, 'M': 18, 'N': 20, 'O': 11, 'P': 3, 'Q': 6, 'R': 8, 'S': 12, 'T': 14,
	'U': 16, 'V': 10, 'W': 22, 'X': 25, 'Y': 24, 'Z': 23,
}

var cfEvenTable = map[byte]int{
	'0': 0, '1': 1, '2': 2, '3': 3, '4': 4, '5': 5, '6': 6, '7': 7, '8': 8, '9': 9,
	'A': 0, 'B': 1, 'C': 2, 'D': 3, 'E': 4, 'F': 5, 'G': 6, 'H': 7, 'I': 8, 'J': 9,
	'K': 10, 'L': 11, 'M': 12, 'N': 13, 'O': 14, 'P': 15, 'Q': 16, 'R': 17, 'S': 18, 'T': 19,
	'U': 20, 'V': 21, 'W': 22, 'X': 23, 'Y': 24, 'Z': 25,
}

// cfCheckDigit computes the 16th character of a codice fiscale from its
// first 15 (already uppercased). Positions are 1-indexed: odd positions
// use cfOddTable, even positions use cfEvenTable; the sum mod 26 selects a
// letter A-Z.
func cfCheckDigit(first15 string) rune {
	sum := 0
	for i := 0; i < len(first15); i++ {
		position := i + 1
		c := first15[i]
		if position%2 == 1 {
			sum += cfOddTable[c]
		} else {
			sum += cfEvenTable[c]
		}
	}
	return rune('A' + sum%26)
}
