package docredact

import (
	"regexp"
	"strconv"
	"strings"
)

// dniPattern matches a Spanish DNI (8 digits + control letter) or NIE
// (X/Y/Z + 7 digits + control letter), with an optional single space or
// hyphen before the letter -- the two conventional ways the control
// letter is set off from the number. Only uppercase letters are matched:
// identity documents always print the control letter (and the NIE's
// leading X/Y/Z) uppercase, and matching lowercase too would just triple
// the false-positive surface for no document ever actually written that
// way.
var dniPattern = regexp.MustCompile(`\b([XYZ]\d{7}|\d{8})[ -]?([A-Z])\b`)

// dniControlLetters is the fixed 23-letter table the Spanish government
// uses: the control letter is this string indexed by the 8-digit number
// mod 23. It is a published constant, not derived from anything.
const dniControlLetters = "TRWAGMYFPDXBNJZSQVHLCKE"

type ESDNIDetector struct{}

func (ESDNIDetector) Name() string { return "es_dni" }

func (ESDNIDetector) Detect(text string) []Match {
	var out []Match
	for _, loc := range dniPattern.FindAllStringSubmatchIndex(text, -1) {
		number := text[loc[2]:loc[3]]
		letter := text[loc[4]:loc[5]]

		digits := number
		if idx := strings.IndexByte("XYZ", number[0]); idx >= 0 {
			// NIE: the leading letter stands in for a leading digit
			// (X=0, Y=1, Z=2) the control-letter formula needs.
			digits = string(rune('0'+idx)) + number[1:]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			continue
		}
		if string(dniControlLetters[n%23]) != letter {
			continue
		}
		out = append(out, Match{
			Start:    loc[0],
			End:      loc[1],
			Text:     text[loc[0]:loc[1]],
			Category: CategoryESDNI,
			Source:   Source,
		})
	}
	return out
}
