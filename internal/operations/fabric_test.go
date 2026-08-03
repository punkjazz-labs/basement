package operations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSysfs builds the kernel's view of a machine with ConnectX ports:
// each entry maps an RDMA device to its network interface and link state.
func fakeSysfs(t *testing.T, ports map[string]struct {
	netdev string
	link   bool
}) string {
	t.Helper()
	root := t.TempDir()
	for hca, port := range ports {
		netDir := filepath.Join(root, "class/infiniband", hca, "device", "net", port.netdev)
		if err := os.MkdirAll(netDir, 0o755); err != nil {
			t.Fatal(err)
		}
		carrier := "0\n"
		if port.link {
			carrier = "1\n"
		}
		carrierPath := filepath.Join(root, "class/net", port.netdev)
		if err := os.MkdirAll(carrierPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(carrierPath, "carrier"), []byte(carrier), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func withSysfs(t *testing.T, root string) {
	t.Helper()
	previous := sysfsRoot
	sysfsRoot = root
	t.Cleanup(func() { sysfsRoot = previous })
}

// The owner cabled port 1, not the port 0 the community recipe pins. The
// kernel's answer wins.
func TestDetectFabricLinkFindsTheCabledPort(t *testing.T) {
	withSysfs(t, fakeSysfs(t, map[string]struct {
		netdev string
		link   bool
	}{
		"rocep1s0f0": {netdev: "enp1s0f0np0", link: false},
		"rocep1s0f1": {netdev: "enp1s0f1np1", link: true},
	}))
	link, err := detectFabricLink()
	if err != nil {
		t.Fatal(err)
	}
	if link.NetDev != "enp1s0f1np1" || link.HCA != "rocep1s0f1" {
		t.Errorf("detected %+v, want the port with link", link)
	}
}

// OEM units split each QSFP cage across two controllers, so one cable
// shows link on two functions. The one holding an IPv4 address wins;
// this is the MSI EdgeXpert's real shape.
func TestDetectFabricLinkBreaksTiesByAddress(t *testing.T) {
	withSysfs(t, fakeSysfs(t, map[string]struct {
		netdev string
		link   bool
	}{
		"rocep1s0f0":   {netdev: "enp1s0f0np0", link: false},
		"rocep1s0f1":   {netdev: "enp1s0f1np1", link: true},
		"roceP2p1s0f0": {netdev: "enP2p1s0f0np0", link: false},
		"roceP2p1s0f1": {netdev: "enP2p1s0f1np1", link: true},
	}))
	previous := interfaceHasIPv4
	t.Cleanup(func() { interfaceHasIPv4 = previous })
	interfaceHasIPv4 = func(name string) bool { return name == "enp1s0f1np1" }

	link, err := detectFabricLink()
	if err != nil {
		t.Fatal(err)
	}
	if link.NetDev != "enp1s0f1np1" || link.HCA != "rocep1s0f1" {
		t.Errorf("detected %+v, want the linked port holding the address", link)
	}

	interfaceHasIPv4 = func(string) bool { return false }
	if _, err := detectFabricLink(); err == nil || !strings.Contains(err.Error(), "none holds an IPv4 address") {
		t.Errorf("linked ports without addresses should say so, got %v", err)
	}

	interfaceHasIPv4 = func(string) bool { return true }
	if _, err := detectFabricLink(); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("two addressed linked ports should be ambiguous, got %v", err)
	}
}

func TestDetectFabricLinkAsksForTheCable(t *testing.T) {
	withSysfs(t, fakeSysfs(t, map[string]struct {
		netdev string
		link   bool
	}{
		"rocep1s0f0": {netdev: "enp1s0f0np0", link: false},
	}))
	if _, err := detectFabricLink(); err == nil || !strings.Contains(err.Error(), "connect the cable") {
		t.Errorf("no linked port should ask for the cable, got %v", err)
	}
}

// Detection beats the recipe's pinned names; the pinned names carry a
// machine where detection has no unambiguous answer.
func TestResolveFabricPrefersDetectionThenFallsBack(t *testing.T) {
	previous := fabricLink
	t.Cleanup(func() { fabricLink = previous })

	fabricLink = func() (FabricLink, error) {
		return FabricLink{NetDev: "enp1s0f1np1", HCA: "rocep1s0f1"}, nil
	}
	link, err := resolveFabric(twoSparkRecipe(t))
	if err != nil {
		t.Fatal(err)
	}
	if link.NetDev != "enp1s0f1np1" || link.HCA != "rocep1s0f1" {
		t.Errorf("resolved %+v, want the detected port", link)
	}

	fabricLink = func() (FabricLink, error) { return FabricLink{}, os.ErrNotExist }
	link, err = resolveFabric(twoSparkRecipe(t))
	if err != nil {
		t.Fatal(err)
	}
	if link.NetDev != "enp1s0f0np0" || link.HCA != "rocep1s0f0" {
		t.Errorf("resolved %+v, want the recipe's pinned fallback", link)
	}
}

// withFabric stubs both halves of live resolution. CI runners can expose a
// real RDMA device, so every test about the cable has to fake the kernel or
// it is testing the machine it happens to run on.
func withFabric(t *testing.T, link FabricLink, detected error, address string, addressErr error) {
	t.Helper()
	previousLink, previousAddress := fabricLink, fabricAddress
	t.Cleanup(func() { fabricLink, fabricAddress = previousLink, previousAddress })
	fabricLink = func() (FabricLink, error) { return link, detected }
	fabricAddress = func(string) (string, error) { return address, addressErr }
}

// The whole point of the check: the worker listens on its own fabric address
// and the head, dialing from its own, gets the worker's token back.
func TestFabricProbeRoundTripsBetweenTheTwoPorts(t *testing.T) {
	withFabric(t, FabricLink{NetDev: "enp1s0f1np1", HCA: "rocep1s0f1"}, nil, "127.0.0.1", nil)
	probe, err := ServeFabricProbe(twoSparkRecipe(t))
	if err != nil {
		t.Fatal(err)
	}
	if probe.Interface != "enp1s0f1np1" || probe.HCA != "rocep1s0f1" || probe.Address != "127.0.0.1" || probe.Port == 0 || probe.Token == "" {
		t.Fatalf("the probe does not say where to meet: %#v", probe)
	}
	if err := dialFabricProbe(context.Background(), "127.0.0.1", probe); err != nil {
		t.Fatalf("the two ports could not meet: %v", err)
	}
}

// A listener bound to the fabric address alone would also be reachable over
// the management LAN if it were bound to every address; this pins that it is
// not, so the check can only ever be passed by the cable.
func TestFabricProbeListensOnlyOnTheFabricAddress(t *testing.T) {
	withFabric(t, FabricLink{NetDev: "enp1s0f1np1"}, nil, "127.0.0.1", nil)
	probe, err := ServeFabricProbe(twoSparkRecipe(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialFabricProbe(context.Background(), "127.0.0.1", probe) })
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip("this machine reports no interface addresses")
	}
	for _, address := range addresses {
		network, ok := address.(*net.IPNet)
		if !ok || network.IP.To4() == nil || network.IP.IsLoopback() {
			continue
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(network.IP.String(), fmt.Sprint(probe.Port)), time.Second)
		if err == nil {
			conn.Close()
			t.Fatalf("the probe answered on %s as well as the fabric address", network.IP)
		}
	}
}

// A token that does not come back means something other than the worker's
// probe answered, which is not proof the cable carries anything.
func TestFabricProbeRefusesAnUnprovenAnswer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_, _ = io.WriteString(conn, "0000000000000000000000000000000f")
		conn.Close()
	}()
	probe := FabricProbe{Interface: "enp1s0f1np1", Address: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Token: "0000000000000000000000000000000a"}
	err = dialFabricProbe(context.Background(), "127.0.0.1", probe)
	if err == nil || !strings.Contains(err.Error(), "cannot reach each other") {
		t.Fatalf("an unproven answer must fail the check, got %v", err)
	}
}

// Nothing listening is the everyday failure: both ends lit, no path between
// them. The sentence has to say exactly that.
func TestFabricProbeSaysWhenThePortsCannotReachEachOther(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	probe := FabricProbe{Interface: "enP2p1s0f1np1", Address: "127.0.0.1", Port: port, Token: "token"}
	err = dialFabricProbe(context.Background(), "127.0.0.1", probe)
	if err == nil || !strings.Contains(err.Error(), "the cable is connected on both Sparks but their high-speed ports cannot reach each other") {
		t.Fatalf("an unreachable port must say so, got %v", err)
	}
	if !strings.Contains(err.Error(), "enP2p1s0f1np1") {
		t.Fatalf("the message must name the port that did not answer, got %v", err)
	}
}

// No cable is reported as no cable, even though the recipe pins names that
// would otherwise be tried and reported as an address problem instead.
func TestFabricEndpointAsksForTheCableRatherThanBlamingTheRecipesPin(t *testing.T) {
	withFabric(t, FabricLink{}, errors.New("no high-speed port has link (rocep1s0f0/enp1s0f0np0: no link); connect the cable between the two Sparks"), "", errors.New("high-speed port enp1s0f0np0 has no address, so the two Sparks have nothing to meet on"))
	_, _, err := fabricEndpoint(twoSparkRecipe(t), fabricAddress)
	if err == nil || !strings.Contains(err.Error(), "connect the cable between the two Sparks") {
		t.Fatalf("a missing cable must ask for the cable, got %v", err)
	}
	if _, err := ServeFabricProbe(twoSparkRecipe(t)); err == nil || !strings.Contains(err.Error(), "connect the cable between the two Sparks") {
		t.Fatalf("the worker must ask for the cable too, got %v", err)
	}
}

// The cable is in and the port is detected, but it holds no address: there is
// nothing for the other Spark to dial.
func TestFabricEndpointSaysWhenThePortHasNoAddress(t *testing.T) {
	withFabric(t, FabricLink{NetDev: "enp1s0f1np1", HCA: "rocep1s0f1"}, nil, "", errors.New("high-speed port enp1s0f1np1 has no address, so the two Sparks have nothing to meet on"))
	_, _, err := fabricEndpoint(twoSparkRecipe(t), fabricAddress)
	if err == nil || !strings.Contains(err.Error(), "has no address, so the two Sparks have nothing to meet on") {
		t.Fatalf("an unaddressed port must say so, got %v", err)
	}
	if _, err := ServeFabricProbe(twoSparkRecipe(t)); err == nil || !strings.Contains(err.Error(), "enp1s0f1np1 has no address") {
		t.Fatalf("the worker must name its unaddressed port, got %v", err)
	}
}

// Detection is preferred, but a machine whose kernel gives no clear answer
// still runs on the recipe's pinned names when those do hold an address.
func TestFabricEndpointFallsBackToThePinnedNames(t *testing.T) {
	withFabric(t, FabricLink{}, errors.New("this machine reports no RDMA devices"), "169.254.9.9", nil)
	link, address, err := fabricEndpoint(twoSparkRecipe(t), fabricAddress)
	if err != nil {
		t.Fatal(err)
	}
	if link.NetDev != "enp1s0f0np0" || address != "169.254.9.9" {
		t.Fatalf("resolved %+v at %s, want the recipe's pinned fallback", link, address)
	}
}

// Each rank's container pins NCCL to that rank's own detected port.
func TestInterconnectEnvironmentUsesDetectedPort(t *testing.T) {
	previous := fabricLink
	t.Cleanup(func() { fabricLink = previous })
	fabricLink = func() (FabricLink, error) {
		return FabricLink{NetDev: "enp1s0f1np1", HCA: "rocep1s0f1"}, nil
	}
	placement := Placement{Role: RoleWorker, NodeCount: 2, MasterAddress: "169.254.10.1", MasterPort: 29501}
	environment := interconnectEnvironment(twoSparkRecipe(t), placement)
	for name, want := range map[string]string{
		"NCCL_SOCKET_IFNAME": "enp1s0f1np1",
		"GLOO_SOCKET_IFNAME": "enp1s0f1np1",
		"TP_SOCKET_IFNAME":   "enp1s0f1np1",
		"NCCL_IB_HCA":        "rocep1s0f1",
	} {
		if environment[name] != want {
			t.Errorf("%s = %q, want %q", name, environment[name], want)
		}
	}
}
