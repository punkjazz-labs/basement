package operations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

// FabricLink is a cabled high-speed port: the RDMA device NCCL drives and
// the network interface whose address the ranks meet on.
type FabricLink struct {
	NetDev string // e.g. enp1s0f1np1 — NCCL/GLOO/TP socket interface
	HCA    string // e.g. rocep1s0f1 — NCCL_IB_HCA
}

// sysfsRoot is the kernel's view of this machine; tests point it at a fixture.
var sysfsRoot = "/sys"

// fabricLink is the detector, injectable for tests.
var fabricLink = detectFabricLink

// interfaceHasIPv4 reports whether the named interface holds an IPv4
// address. Injectable because tests fake sysfs but cannot fake netlink.
var interfaceHasIPv4 = func(name string) bool {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return false
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if network, ok := addr.(*net.IPNet); ok && network.IP.To4() != nil {
			return true
		}
	}
	return false
}

// detectFabricLink finds the ConnectX port the owner actually cabled: the
// RDMA device whose network interface has carrier. The owner plugs the
// cable into whichever port is convenient, so the kernel, not a recipe,
// knows which one they chose. Exactly one port with link is the answer;
// none or several is an error that names what was found instead.
func detectFabricLink() (FabricLink, error) {
	devices, err := os.ReadDir(filepath.Join(sysfsRoot, "class/infiniband"))
	if err != nil {
		return FabricLink{}, errors.New("this machine reports no RDMA devices")
	}
	var linked []FabricLink
	var seen []string
	for _, device := range devices {
		hca := device.Name()
		nets, err := os.ReadDir(filepath.Join(sysfsRoot, "class/infiniband", hca, "device", "net"))
		if err != nil || len(nets) == 0 {
			continue
		}
		netdev := nets[0].Name()
		carrier, err := os.ReadFile(filepath.Join(sysfsRoot, "class/net", netdev, "carrier"))
		if err == nil && strings.TrimSpace(string(carrier)) == "1" {
			linked = append(linked, FabricLink{NetDev: netdev, HCA: hca})
			seen = append(seen, hca+"/"+netdev+": link")
		} else {
			seen = append(seen, hca+"/"+netdev+": no link")
		}
	}
	if len(linked) == 0 {
		return FabricLink{}, fmt.Errorf("no high-speed port has link (%s); connect the cable between the two Sparks", strings.Join(seen, ", "))
	}
	if len(linked) == 1 {
		return linked[0], nil
	}
	// One cable can light more than one function: OEM units split each QSFP
	// cage across two controllers, so a single cable shows link twice. The
	// port holding an IPv4 address is the one the fabric actually meets on,
	// and the same address becomes --master-addr on the head.
	var addressed []FabricLink
	for _, candidate := range linked {
		if interfaceHasIPv4(candidate.NetDev) {
			addressed = append(addressed, candidate)
		}
	}
	switch len(addressed) {
	case 1:
		return addressed[0], nil
	case 0:
		return FabricLink{}, fmt.Errorf("more than one high-speed port has link (%s) and none holds an IPv4 address, so there is nothing to meet on", strings.Join(seen, ", "))
	default:
		return FabricLink{}, fmt.Errorf("more than one high-speed port has link and an IPv4 address (%s), so the cabling is ambiguous", strings.Join(seen, ", "))
	}
}

// resolveFabric picks the fabric names for THIS node: the port the kernel
// says is cabled when that answer is unambiguous, else the names the recipe
// pins. The pinned names are the fallback, not the primary, because the two
// machines may not even have the cable in the same port as each other; each
// rank pins its transports to its own detected port and they still meet on
// the head's address.
func resolveFabric(r recipe.Recipe) (FabricLink, error) {
	link, err := fabricLink()
	if err == nil {
		return link, nil
	}
	if name := r.Topology.SocketInterface(); name != "" {
		return FabricLink{NetDev: name, HCA: r.Topology.Interconnect.SharedEnvironment["NCCL_IB_HCA"]}, nil
	}
	return FabricLink{}, err
}

// fabricAddress resolves an interface's own IPv4 address. Injectable for the
// same reason interfaceHasIPv4 is: tests can fake sysfs but not netlink.
var fabricAddress = FabricAddress

// fabricEndpoint is this node's side of the cable, resolved live: the port to
// meet on and the address it holds at this moment. Nothing here is read from
// a store or from a previous job, which is what makes an address the kernel
// happens to assign differently after a reboot a non-event.
//
// When the port cannot be addressed and the kernel also saw no link, the link
// is the honest reason and the one reported. The recipe's pinned names are
// still the fallback for a machine whose kernel gives no clear answer, but a
// fallback name that holds no address must not be allowed to turn "the cable
// is not in" into "enp1s0f0np0 has no address", which tells the owner nothing
// they can act on.
func fabricEndpoint(r recipe.Recipe, address func(string) (string, error)) (FabricLink, string, error) {
	link, detected := fabricLink()
	if detected != nil {
		fallback, err := resolveFabric(r)
		if err != nil {
			return FabricLink{}, "", err
		}
		link = fallback
	}
	held, err := address(link.NetDev)
	if err != nil {
		if detected != nil {
			return FabricLink{}, "", detected
		}
		return FabricLink{}, "", err
	}
	return link, held, nil
}

// VerifyFabric is an engine-generated step, never a recipe operation: before
// a single byte is downloaded or pulled, the two Sparks prove they can meet
// over the cable. It is planned first in every distributed job because every
// later step of that job is wasted work if they cannot.
const VerifyFabric = "verify_fabric"

// FabricProbe is what the worker answers with when the head asks it where on
// the cable it can be met: the port it detected, the address that port holds
// right now, an ephemeral port it is listening on for exactly one connection,
// and a token proving the connection reached that listener and not something
// else that happens to answer on the same address.
type FabricProbe struct {
	Interface string `json:"interface"`
	HCA       string `json:"hca,omitempty"`
	Address   string `json:"address"`
	Port      int    `json:"port"`
	Token     string `json:"token"`
}

// fabricProbeTTL bounds how long the worker holds its listener open. The head
// dials the moment it has the answer, so this only has to cover one call's
// round trip; a head that never dials must not leave a socket behind.
const fabricProbeTTL = 30 * time.Second

// fabricDialTimeout bounds the head's dial. A cable that is seated but not
// carrying traffic between these two ports fails by timing out rather than by
// being refused, so this is what the owner actually waits for on a bad link.
const fabricDialTimeout = 10 * time.Second

// ServeFabricProbe opens this node's side of the reachability check. It is a
// variable so the node endpoint that exposes it can be tested without the
// machine running the test needing a cable in it.
var ServeFabricProbe = serveFabricProbe

// serveFabricProbe re-resolves the cabled port and its address live, then
// listens on that address alone. Binding the fabric address (not every
// address) is the point: a listener on 0.0.0.0 would also answer over the
// management LAN, which proves nothing about the cable.
func serveFabricProbe(r recipe.Recipe) (FabricProbe, error) {
	link, address, err := fabricEndpoint(r, fabricAddress)
	if err != nil {
		return FabricProbe{}, err
	}
	token, err := fabricToken()
	if err != nil {
		return FabricProbe{}, err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	if err != nil {
		return FabricProbe{}, fmt.Errorf("it could not open a port on its high-speed link at %s", address)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	go answerFabricProbe(listener, token)
	return FabricProbe{Interface: link.NetDev, HCA: link.HCA, Address: address, Port: port, Token: token}, nil
}

// answerFabricProbe serves exactly one connection and then closes the
// listener, so a probe can never be left open as a standing service.
func answerFabricProbe(listener net.Listener, token string) {
	defer listener.Close()
	if tcp, ok := listener.(*net.TCPListener); ok {
		_ = tcp.SetDeadline(time.Now().Add(fabricProbeTTL))
	}
	conn, err := listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(fabricProbeTTL))
	_, _ = io.WriteString(conn, token)
}

func fabricToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("this Spark could not generate a token for the cable check")
	}
	return hex.EncodeToString(raw), nil
}

// dialFabricProbe is the head's half: it connects from its own fabric address
// to the worker's, so the traffic is forced onto the cable rather than
// allowed to find the machines over the management LAN, and it requires the
// worker's token back before it calls the link proven.
func dialFabricProbe(ctx context.Context, localAddress string, probe FabricProbe) error {
	local := net.ParseIP(localAddress)
	if local == nil {
		return fmt.Errorf("this Spark's high-speed address %s is not usable", localAddress)
	}
	dialer := &net.Dialer{Timeout: fabricDialTimeout, LocalAddr: &net.TCPAddr{IP: local}}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(probe.Address, fmt.Sprint(probe.Port)))
	if err != nil {
		return errCableUnreachable(localAddress, probe)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(fabricDialTimeout))
	answer, err := io.ReadAll(io.LimitReader(conn, int64(len(probe.Token))))
	if err != nil || string(answer) != probe.Token {
		return errCableUnreachable(localAddress, probe)
	}
	return nil
}

func errCableUnreachable(localAddress string, probe FabricProbe) error {
	return fmt.Errorf("the cable is connected on both Sparks but their high-speed ports cannot reach each other: this Spark at %s got no answer from %s at %s; check that both ends of the cable are in the ports that show a link", localAddress, probe.Interface, probe.Address)
}
