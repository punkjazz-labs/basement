package docredact

import "testing"

func TestCategoryPrefix(t *testing.T) {
	cases := map[Category]string{
		CategoryEmail:     "EMAIL",
		CategoryPhone:     "PHONE",
		CategoryIBAN:      "IBAN",
		CategoryCard:      "CARD",
		CategoryIPv4:      "IPV4",
		CategoryIPv6:      "IPV6",
		CategoryDOB:       "DOB",
		CategoryITCF:      "ITCF",
		CategoryUSSSN:     "SSN",
		Category("bogus"): "MATCH",
	}
	for category, want := range cases {
		if got := category.Prefix(); got != want {
			t.Errorf("%s.Prefix() = %q, want %q", category, got, want)
		}
	}
}

func TestLocalesRegistered(t *testing.T) {
	want := []string{"IT", "US"}
	if len(Locales) != len(want) {
		t.Fatalf("Locales = %v, want %v", Locales, want)
	}
	for i, locale := range want {
		if Locales[i] != locale {
			t.Errorf("Locales[%d] = %q, want %q", i, Locales[i], locale)
		}
	}
}

// TestRegistryNamesAreUnique guards against a copy-paste detector that
// silently shadows another one's Name(), which would make detector-level
// debugging output ambiguous.
func TestRegistryNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Registry() {
		if seen[d.Name()] {
			t.Errorf("detector name %q registered more than once", d.Name())
		}
		seen[d.Name()] = true
	}
}

func TestDetectAllTagsSourcePattern(t *testing.T) {
	for _, m := range DetectAll(corpus) {
		if m.Source != "pattern" {
			t.Errorf("match %q has source %q, want \"pattern\"", m.Text, m.Source)
		}
	}
}

func TestIsCalendarDateRejectsImpossibleDates(t *testing.T) {
	cases := []struct {
		day, month, year int
		want             bool
	}{
		{29, 2, 2000, true},  // 2000 is a leap year
		{29, 2, 1999, false}, // 1999 is not
		{30, 4, 2020, true},
		{31, 4, 2020, false}, // April has 30 days
		{15, 13, 2020, false},
		{0, 5, 2020, false},
	}
	for _, c := range cases {
		if got := isCalendarDate(c.day, c.month, c.year); got != c.want {
			t.Errorf("isCalendarDate(%d, %d, %d) = %v, want %v", c.day, c.month, c.year, got, c.want)
		}
	}
}

func TestLuhnValid(t *testing.T) {
	if !luhnValid("4111111111111111") {
		t.Error("4111111111111111 should be Luhn-valid")
	}
	if luhnValid("4111111111111112") {
		t.Error("4111111111111112 should be Luhn-invalid")
	}
}

func TestValidSSNRanges(t *testing.T) {
	cases := []struct {
		area, group, serial int
		want                bool
	}{
		{219, 9, 9999, true},
		{0, 9, 9999, false},
		{666, 9, 9999, false},
		{900, 9, 9999, false},
		{219, 0, 9999, false},
		{219, 9, 0, false},
	}
	for _, c := range cases {
		if got := validSSN(c.area, c.group, c.serial); got != c.want {
			t.Errorf("validSSN(%d, %d, %d) = %v, want %v", c.area, c.group, c.serial, got, c.want)
		}
	}
}
