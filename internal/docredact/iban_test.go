package docredact

import "testing"

// ibanTestCases pairs a country with a literal built by a throwaway
// script before this test was committed: pick a BBAN (all digits, or for
// NL/GB a bank-code-shaped letter+digit prefix), then solve for the two
// check digits that make the ISO 7064 mod-97-10 check land on the
// required remainder of 1. Each literal is exactly the ibanLength value
// for its country, so both isValidIBAN's length gate and its mod-97
// check must pass for the test to be meaningful.
var ibanTestCases = []struct {
	country string
	literal string
}{
	{"FR", "FR0811111111111111111111111"},
	{"DE", "DE63111111111111111111"},
	{"ES", "ES6011111111111111111111"},
	{"PT", "PT24111111111111111111111"},
	{"NL", "NL31ABNA1234567890"},
	{"GB", "GB60NWBK12345678123456"},
	{"BR", "BR961111111111111111111111111"},
}

// TestIBANValidPerCountry proves each length entry the brief asked for
// (FR, DE, ES, PT, NL, GB, BR) actually detects a real, checksum-valid
// IBAN of that country's length.
func TestIBANValidPerCountry(t *testing.T) {
	for _, c := range ibanTestCases {
		wantLen, ok := ibanLength[c.country]
		if !ok {
			t.Fatalf("%s: not present in ibanLength", c.country)
		}
		if len(c.literal) != wantLen {
			t.Fatalf("%s: literal %q has length %d, want %d", c.country, c.literal, len(c.literal), wantLen)
		}
		text := "IBAN on file: " + c.literal + "."
		matches := IBANDetector{}.Detect(text)
		if len(matches) != 1 {
			t.Fatalf("%s: got %d matches, want 1: %+v", c.country, len(matches), matches)
		}
		if matches[0].Text != c.literal {
			t.Errorf("%s: Text = %q, want %q", c.country, matches[0].Text, c.literal)
		}
		if matches[0].Category != CategoryIBAN {
			t.Errorf("%s: Category = %q, want %q", c.country, matches[0].Category, CategoryIBAN)
		}
	}
}

func TestIBANLengthTableHasAddedCountries(t *testing.T) {
	want := map[string]int{
		"FR": 27, "DE": 22, "ES": 24, "PT": 25, "NL": 18, "GB": 22, "BR": 29,
	}
	for country, length := range want {
		got, ok := ibanLength[country]
		if !ok {
			t.Errorf("%s missing from ibanLength", country)
			continue
		}
		if got != length {
			t.Errorf("ibanLength[%q] = %d, want %d", country, got, length)
		}
	}
}

// TestIBANWrongLengthForCountryRejected is the FR literal above with one
// digit removed from the middle: same country prefix, no longer the
// required 27 characters, so isValidIBAN must reject it on length alone
// regardless of what the mod-97 check would say.
func TestIBANWrongLengthForCountryRejected(t *testing.T) {
	text := "IBAN FR081111111111111111111111 (one digit short) is invalid."
	matches := IBANDetector{}.Detect(text)
	if len(matches) != 0 {
		t.Errorf("expected no match for wrong-length IBAN, got %+v", matches)
	}
}
