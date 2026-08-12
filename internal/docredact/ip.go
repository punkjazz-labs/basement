package docredact

import (
	"net"
	"regexp"
	"strings"
)

// ipv4Pattern matches four dot-separated groups of 1-3 digits. It over-
// matches slightly (e.g. "999.999.999.999" is shape-correct) so every
// candidate still goes through net.ParseIP, which is the real validator.
var ipv4Pattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

type IPv4Detector struct{}

func (IPv4Detector) Name() string { return "ipv4" }

func (IPv4Detector) Detect(text string) []Match {
	return matchesFromRegexp(text, ipv4Pattern, CategoryIPv4, func(s string) bool {
		ip := net.ParseIP(s)
		return ip != nil && ip.To4() != nil
	})
}

// ipv6Candidate matches a maximal run of hex digits and colons. Real IPv6
// addresses in prose are surrounded by whitespace or punctuation that is
// not in this character class, so the run naturally stops at the address
// boundary; net.ParseIP is still the authority on whether it is valid.
var ipv6Candidate = regexp.MustCompile(`[0-9A-Fa-f:]+`)

type IPv6Detector struct{}

func (IPv6Detector) Name() string { return "ipv6" }

func (IPv6Detector) Detect(text string) []Match {
	var out []Match
	for _, loc := range ipv6Candidate.FindAllStringIndex(text, -1) {
		literal := text[loc[0]:loc[1]]
		// Require at least two colons: this is what separates an IPv6
		// candidate from a bare hex number or a time-of-day like "12:30".
		if strings.Count(literal, ":") < 2 {
			continue
		}
		ip := net.ParseIP(literal)
		if ip == nil || ip.To4() != nil {
			continue
		}
		out = append(out, Match{
			Start:    loc[0],
			End:      loc[1],
			Text:     literal,
			Category: CategoryIPv6,
			Source:   Source,
		})
	}
	return out
}
