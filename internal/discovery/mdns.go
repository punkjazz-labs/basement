package discovery

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// mDNS one-shot queries (RFC 6762 §5.1): we ask from an ephemeral port with
// the unicast-response bit set, so responders reply directly to us and we
// never need to own port 5353 alongside avahi or mDNSResponder.

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

type mdnsService struct {
	ip       net.IP
	hostname string
}

// maxMDNSServices caps one browse. mDNS is unauthenticated broadcast: anything
// on the segment can answer, as often as it likes, with as many records as it
// likes. A sweep is a convenience, so it stops collecting long before a flood
// costs this machine anything.
const maxMDNSServices = 128

// browseMDNS asks for PTR records of a service type (e.g. "_ssh._tcp.local.")
// and returns every announced instance with its host address.
func browseMDNS(ctx context.Context, service string) []mdnsService {
	collector := &mdnsCollector{seen: map[string]bool{}}
	mdnsExchange(ctx, service, dnsmessage.TypePTR, collector.accept)
	return collector.services
}

// mdnsCollector turns answers into candidates, capped and deduplicated.
type mdnsCollector struct {
	services []mdnsService
	seen     map[string]bool
}

func (c *mdnsCollector) accept(payload []byte, from net.Addr) {
	if len(c.services) >= maxMDNSServices {
		return
	}
	udp, ok := from.(*net.UDPAddr)
	if !ok {
		return
	}
	hostname, advertised := parseServiceAnnouncement(payload)
	ip := acceptedMDNSAddress(advertised, udp.IP)
	if ip == nil || c.seen[ip.String()] {
		return
	}
	c.seen[ip.String()] = true
	c.services = append(c.services, mdnsService{ip: ip, hostname: hostname})
}

// acceptedMDNSAddress decides which address, if any, one mDNS answer may put
// on the candidate list. An A record is a claim the responder makes, not a
// fact: a machine on the segment can advertise any address at all, and every
// address that reaches the list is one this manager will then connect to. So
// only the address the answer actually came from is ever used, and only when
// it is on a network the owner could own. An advertisement naming a different
// address is dropped whole rather than half-believed, and nothing is said
// about it: the text came from the same responder.
func acceptedMDNSAddress(advertised, source net.IP) net.IP {
	source = source.To4()
	if source == nil || !IsLocalFabric(source) {
		return nil
	}
	if advertised != nil && !advertised.Equal(source) {
		return nil
	}
	return source
}

// reverseMDNS resolves an IP back to the .local hostname avahi publishes.
func reverseMDNS(ctx context.Context, ip net.IP) string {
	var hostname string
	brief, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	mdnsExchange(brief, reverseName(ip), dnsmessage.TypePTR, func(payload []byte, _ net.Addr) {
		if name := parseFirstPTR(payload); name != "" {
			hostname = name
			cancel()
		}
	})
	return hostname
}

// mdnsExchange sends the query to the mDNS group and hands every response
// payload to handle until the context expires.
func mdnsExchange(ctx context.Context, name string, qtype dnsmessage.Type, handle func(payload []byte, from net.Addr)) {
	packed, err := packQuery(name, qtype)
	if err != nil {
		return
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.SetReadDeadline(time.Now())
	}()
	conn.WriteToUDP(packed, mdnsGroup)
	resend := time.AfterFunc(400*time.Millisecond, func() { conn.WriteToUDP(packed, mdnsGroup) })
	defer resend.Stop()
	buffer := make([]byte, 9000)
	for {
		if deadline, ok := ctx.Deadline(); ok {
			conn.SetReadDeadline(deadline)
		}
		size, from, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		payload := make([]byte, size)
		copy(payload, buffer[:size])
		handle(payload, from)
	}
}

func packQuery(name string, qtype dnsmessage.Type) ([]byte, error) {
	parsed, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, err
	}
	message := dnsmessage.Message{
		Questions: []dnsmessage.Question{{
			Name:  parsed,
			Type:  qtype,
			Class: dnsmessage.Class(0x8001), // IN with the unicast-response bit
		}},
	}
	return message.Pack()
}

// parseServiceAnnouncement extracts the announced host (.local name) and its
// IPv4 address from a service-browse response: PTR gives the instance, SRV
// names the target host, A carries the address.
func parseServiceAnnouncement(payload []byte) (hostname string, ip net.IP) {
	var message dnsmessage.Message
	if err := message.Unpack(payload); err != nil {
		return "", nil
	}
	records := append(message.Answers, message.Additionals...)
	for _, record := range records {
		if srv, ok := record.Body.(*dnsmessage.SRVResource); ok && hostname == "" {
			hostname = strings.TrimSuffix(srv.Target.String(), ".")
		}
	}
	for _, record := range records {
		if a, ok := record.Body.(*dnsmessage.AResource); ok {
			candidate := net.IP(a.A[:])
			// Prefer the A record matching the SRV target when present.
			if hostname == "" || strings.EqualFold(strings.TrimSuffix(record.Header.Name.String(), "."), hostname) {
				return hostname, candidate
			}
			if ip == nil {
				ip = candidate
			}
		}
	}
	return hostname, ip
}

// parseFirstPTR returns the first PTR answer's target name.
func parseFirstPTR(payload []byte) string {
	var message dnsmessage.Message
	if err := message.Unpack(payload); err != nil {
		return ""
	}
	for _, record := range message.Answers {
		if ptr, ok := record.Body.(*dnsmessage.PTRResource); ok {
			return strings.TrimSuffix(ptr.PTR.String(), ".")
		}
	}
	return ""
}

// reverseName renders the in-addr.arpa question name for an IPv4 address.
func reverseName(ip net.IP) string {
	v4 := ip.To4()
	parts := make([]string, 4)
	for index, octet := range v4 {
		parts[3-index] = strconv.Itoa(int(octet))
	}
	return strings.Join(parts, ".") + ".in-addr.arpa."
}
