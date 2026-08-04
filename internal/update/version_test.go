package update

import "testing"

func TestIsNewerUsesSemanticVersionOrdering(t *testing.T) {
	tests := []struct {
		name      string
		current   string
		candidate string
		want      bool
	}{
		{name: "newer local build is not offered a downgrade", current: "v2.0.0", candidate: "v1.12.9", want: false},
		{name: "equal release is current", current: "v1.4.2", candidate: "v1.4.2", want: false},
		{name: "older local release gets the update", current: "v1.9.9", candidate: "v1.10.0", want: true},
		{name: "development build is not installable", current: "dev", candidate: "v1.10.0", want: false},
		{name: "non-semver build is not installable", current: "build-42", candidate: "v1.10.0", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsNewer(test.current, test.candidate); got != test.want {
				t.Fatalf("IsNewer(%q, %q) = %v, want %v", test.current, test.candidate, got, test.want)
			}
		})
	}
}

func TestParseVersionRejectsNonCanonicalForms(t *testing.T) {
	for _, value := range []string{"1.2.3", "v1.2", "v1.2.3.4", "v01.2.3", "v1.02.3", "v1.2.03", "v1.2.3-rc1", "v1.2.3+build"} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseVersion(value); err == nil {
				t.Fatalf("ParseVersion(%q) succeeded", value)
			}
		})
	}
}
