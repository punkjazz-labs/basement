package discovery

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestLikelyGB10NameMatchesVendorDefaults(t *testing.T) {
	likely := []string{"spark-head.local", "GX10-office.local", "ascent-lab", "EdgeXpert-3", "aitop-atom", "veriton-gn100", "pgx-7", "dell-promax", "my-gb10-box"}
	for _, name := range likely {
		if !LikelyGB10Name(name) {
			t.Errorf("expected %q to rank as likely GB10", name)
		}
	}
	unlikely := []string{"", "macbook-pro.local", "nas", "ubuntu-server"}
	for _, name := range unlikely {
		if LikelyGB10Name(name) {
			t.Errorf("did not expect %q to rank as likely GB10", name)
		}
	}
}

func TestReverseName(t *testing.T) {
	if got := reverseName(net.IPv4(192, 168, 99, 134)); got != "134.99.168.192.in-addr.arpa." {
		t.Fatalf("reverseName = %q", got)
	}
}

func TestHostsInEnumeratesSlash24(t *testing.T) {
	_, network, _ := net.ParseCIDR("192.168.99.0/24")
	hosts := hostsIn(network)
	if len(hosts) != 254 {
		t.Fatalf("want 254 hosts, got %d", len(hosts))
	}
	if hosts[0].String() != "192.168.99.1" || hosts[253].String() != "192.168.99.254" {
		t.Fatalf("unexpected range %s..%s", hosts[0], hosts[253])
	}
}

func mustName(t *testing.T, name string) dnsmessage.Name {
	t.Helper()
	parsed, err := dnsmessage.NewName(name)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestParseServiceAnnouncement(t *testing.T) {
	message := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true, Authoritative: true},
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: mustName(t, "_ssh._tcp.local."), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET},
			Body:   &dnsmessage.PTRResource{PTR: mustName(t, "spark-head._ssh._tcp.local.")},
		}},
		Additionals: []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{Name: mustName(t, "spark-head._ssh._tcp.local."), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.SRVResource{Target: mustName(t, "spark-head.local."), Port: 22},
			},
			{
				Header: dnsmessage.ResourceHeader{Name: mustName(t, "spark-head.local."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.AResource{A: [4]byte{192, 168, 99, 134}},
			},
		},
	}
	payload, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	hostname, ip := parseServiceAnnouncement(payload)
	if hostname != "spark-head.local" {
		t.Errorf("hostname = %q", hostname)
	}
	if ip == nil || ip.String() != "192.168.99.134" {
		t.Errorf("ip = %v", ip)
	}
}

func TestParseFirstPTR(t *testing.T) {
	message := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true},
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: mustName(t, "134.99.168.192.in-addr.arpa."), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET},
			Body:   &dnsmessage.PTRResource{PTR: mustName(t, "gx10-office.local.")},
		}},
	}
	payload, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if got := parseFirstPTR(payload); got != "gx10-office.local" {
		t.Errorf("parseFirstPTR = %q", got)
	}
	if got := parseFirstPTR([]byte{0x01, 0x02}); got != "" {
		t.Errorf("garbage payload should parse to empty, got %q", got)
	}
}

func TestPackQuerySetsUnicastResponseBit(t *testing.T) {
	payload, err := packQuery("_ssh._tcp.local.", dnsmessage.TypePTR)
	if err != nil {
		t.Fatal(err)
	}
	var message dnsmessage.Message
	if err := message.Unpack(payload); err != nil {
		t.Fatal(err)
	}
	if len(message.Questions) != 1 {
		t.Fatalf("want 1 question, got %d", len(message.Questions))
	}
	if message.Questions[0].Class&0x8000 == 0 {
		t.Error("unicast-response bit is not set")
	}
}

// announcement packs one service response the way avahi would, with the
// advertised A record under the caller's control.
func announcement(t *testing.T, host string, advertised [4]byte) []byte {
	t.Helper()
	message := dnsmessage.Message{
		Header:  dnsmessage.Header{Response: true, Authoritative: true},
		Answers: []dnsmessage.Resource{},
		Additionals: []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{Name: mustName(t, host+"._ssh._tcp.local."), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.SRVResource{Target: mustName(t, host+".local."), Port: 22},
			},
			{
				Header: dnsmessage.ResourceHeader{Name: mustName(t, host+".local."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.AResource{A: advertised},
			},
		},
	}
	payload, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// An mDNS answer is an unauthenticated claim from whoever is on the segment.
// Only the address it actually came from may become a candidate, and only
// when that address is on a network the owner could own: otherwise anyone on
// the LAN can make this manager connect to any host on the internet just by
// advertising it.
func TestAcceptedMDNSAddressTrustsOnlyTheResponder(t *testing.T) {
	cases := []struct {
		name       string
		advertised net.IP
		source     net.IP
		want       string
	}{
		{"advertised matches the responder", net.ParseIP("192.168.99.137"), net.ParseIP("192.168.99.137"), "192.168.99.137"},
		{"no address advertised falls back to the responder", nil, net.ParseIP("192.168.99.137"), "192.168.99.137"},
		{"public address advertised", net.ParseIP("203.0.113.9"), net.ParseIP("192.168.99.137"), ""},
		{"another machine's address advertised", net.ParseIP("192.168.99.1"), net.ParseIP("192.168.99.137"), ""},
		{"responder itself is public", net.ParseIP("203.0.113.9"), net.ParseIP("203.0.113.9"), ""},
		{"responder is loopback", nil, net.ParseIP("127.0.0.1"), ""},
		{"tailnet responder", nil, net.ParseIP("100.64.10.5"), "100.64.10.5"},
	}
	for _, test := range cases {
		got := acceptedMDNSAddress(test.advertised, test.source)
		rendered := ""
		if got != nil {
			rendered = got.String()
		}
		if rendered != test.want {
			t.Errorf("%s: acceptedMDNSAddress = %q, want %q", test.name, rendered, test.want)
		}
	}
}

// A flood of announcements is a resource-exhaustion primitive for anyone on
// the segment, because every candidate becomes work later. Collection stops.
func TestMDNSCollectorDropsForeignAddressesAndCapsTheFlood(t *testing.T) {
	collector := &mdnsCollector{seen: map[string]bool{}}
	responder := &net.UDPAddr{IP: net.ParseIP("192.168.99.137"), Port: 5353}
	collector.accept(announcement(t, "evil", [4]byte{203, 0, 113, 9}), responder)
	if len(collector.services) != 0 {
		t.Fatalf("an advertised public address became a candidate: %+v", collector.services)
	}
	for octet := 1; octet < 250; octet++ {
		from := &net.UDPAddr{IP: net.IPv4(192, 168, 99, byte(octet)), Port: 5353}
		collector.accept(announcement(t, "flood", [4]byte{192, 168, 99, byte(octet)}), from)
	}
	if len(collector.services) > maxMDNSServices {
		t.Fatalf("collected %d services, want at most %d", len(collector.services), maxMDNSServices)
	}
	if len(collector.services) != maxMDNSServices {
		t.Errorf("the cap did not engage: %d services", len(collector.services))
	}
}

func TestIsLocalFabric(t *testing.T) {
	local := []string{"192.168.99.137", "10.1.2.3", "172.16.0.9", "169.254.1.1", "100.64.10.5", "fe80::1", "fd00::1"}
	for _, address := range local {
		if !IsLocalFabric(net.ParseIP(address)) {
			t.Errorf("%s should read as local fabric", address)
		}
	}
	foreign := []string{"203.0.113.9", "8.8.8.8", "127.0.0.1", "::1", "0.0.0.0", "2606:4700::1111"}
	for _, address := range foreign {
		if IsLocalFabric(net.ParseIP(address)) {
			t.Errorf("%s should not read as local fabric", address)
		}
	}
	if IsLocalFabric(nil) {
		t.Error("no address is not local fabric")
	}
}
