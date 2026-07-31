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
