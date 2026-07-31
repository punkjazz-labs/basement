// Package discovery finds GB10-class machines (NVIDIA DGX Spark and OEM
// equivalents) on the local network so setup can offer them for installation.
//
// Strategy: candidates come from a parallel TCP:22 sweep of the local /24
// networks plus an mDNS browse for SSH services; each candidate is then given
// a friendly .local hostname via reverse mDNS. Vendor hostname patterns only
// RANK the list — identity is confirmed later over SSH (nvidia-smi reports the
// GB10 chip), because OEM machines ship with vendor-specific default names.
package discovery

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// Candidate is one SSH-reachable machine on the local network.
type Candidate struct {
	IP       net.IP
	Hostname string // mDNS .local name when known, else empty
}

// DisplayName renders the friendliest stable identifier we have.
func (c Candidate) DisplayName() string {
	if c.Hostname != "" {
		return c.Hostname
	}
	return c.IP.String()
}

// gb10HostnameHints are default-hostname fragments of known GB10 products.
// They order the candidate list only; machines matching none still appear,
// and SSH identity probing has the final word.
var gb10HostnameHints = []string{
	"spark",     // NVIDIA DGX Spark
	"gx10",      // ASUS Ascent GX10
	"ascent",    // ASUS Ascent
	"edgexpert", // MSI EdgeXpert
	"aitop",     // Gigabyte AI TOP Atom
	"veriton",   // Acer Veriton GN100
	"pgx",       // Lenovo ThinkStation PGX
	"promax",    // Dell Pro Max with GB10
	"gb10",      // generic
}

// LikelyGB10Name reports whether a hostname matches a known GB10 product's
// default naming, for ranking.
func LikelyGB10Name(hostname string) bool {
	lower := strings.ToLower(hostname)
	for _, hint := range gb10HostnameHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// Discover sweeps the local IPv4 /24 networks for SSH hosts, merges in mDNS
// SSH announcements, resolves reverse mDNS hostnames, and returns candidates
// with likely GB10 machines first.
func Discover(ctx context.Context, logf func(format string, args ...any)) ([]Candidate, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	networks, self, err := localNetworks()
	if err != nil {
		return nil, err
	}
	if len(networks) == 0 {
		return nil, fmt.Errorf("no active IPv4 network interface found")
	}

	found := make(map[string]*Candidate)
	var mu sync.Mutex
	add := func(ip net.IP, hostname string) {
		if ip == nil || ip.To4() == nil || self[ip.String()] {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		entry, ok := found[ip.String()]
		if !ok {
			entry = &Candidate{IP: ip}
			found[ip.String()] = entry
		}
		if hostname != "" && entry.Hostname == "" {
			entry.Hostname = strings.TrimSuffix(hostname, ".")
		}
	}

	sweepCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, network := range networks {
		logf("scanning %s for SSH hosts", network)
		for _, ip := range hostsIn(network) {
			wg.Add(1)
			go func(ip net.IP) {
				defer wg.Done()
				if sshOpen(sweepCtx, ip) {
					add(ip, "")
				}
			}(ip)
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, service := range browseMDNS(sweepCtx, "_ssh._tcp.local.") {
			add(service.ip, service.hostname)
		}
	}()
	wg.Wait()

	// Reverse mDNS gives the friendly .local names avahi publishes.
	nameCtx, cancelNames := context.WithTimeout(ctx, 2*time.Second)
	defer cancelNames()
	var nameWG sync.WaitGroup
	for _, entry := range found {
		if entry.Hostname != "" {
			continue
		}
		nameWG.Add(1)
		go func(entry *Candidate) {
			defer nameWG.Done()
			if name := reverseMDNS(nameCtx, entry.IP); name != "" {
				mu.Lock()
				entry.Hostname = strings.TrimSuffix(name, ".")
				mu.Unlock()
			}
		}(entry)
	}
	nameWG.Wait()

	candidates := make([]Candidate, 0, len(found))
	for _, entry := range found {
		candidates = append(candidates, *entry)
	}
	sort.Slice(candidates, func(i, j int) bool {
		likelyI, likelyJ := LikelyGB10Name(candidates[i].Hostname), LikelyGB10Name(candidates[j].Hostname)
		if likelyI != likelyJ {
			return likelyI
		}
		return candidates[i].IP.String() < candidates[j].IP.String()
	})
	return candidates, nil
}

// localNetworks returns every up, non-loopback IPv4 /24 (or smaller) network
// this machine sits on, plus the set of our own addresses to exclude.
func localNetworks() ([]*net.IPNet, map[string]bool, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}
	var networks []*net.IPNet
	self := map[string]bool{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil {
				continue
			}
			self[ipNet.IP.String()] = true
			ones, bits := ipNet.Mask.Size()
			if bits != 32 || ones < 24 {
				// Cap the sweep at /24 around our address; larger networks
				// are too big to sweep politely.
				ones = 24
			}
			masked := &net.IPNet{IP: ipNet.IP.Mask(net.CIDRMask(ones, 32)), Mask: net.CIDRMask(ones, 32)}
			if tailscaleRange.Contains(masked.IP) {
				continue // Tailscale is point-to-point; sweeping it is noise.
			}
			networks = append(networks, masked)
		}
	}
	return networks, self, nil
}

// hostsIn enumerates usable host addresses of a (at most /24) network.
func hostsIn(network *net.IPNet) []net.IP {
	base := network.IP.To4()
	ones, _ := network.Mask.Size()
	count := 1 << (32 - ones)
	ips := make([]net.IP, 0, count)
	for offset := 1; offset < count-1; offset++ {
		ip := make(net.IP, 4)
		copy(ip, base)
		ip[3] = base[3] + byte(offset)
		ips = append(ips, ip)
	}
	return ips
}

func sshOpen(ctx context.Context, ip net.IP) bool {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), "22"))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

var tailscaleRange = func() *net.IPNet {
	_, block, _ := net.ParseCIDR("100.64.0.0/10")
	return block
}()
