package operations

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

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
