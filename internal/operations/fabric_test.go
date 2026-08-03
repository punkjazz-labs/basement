package operations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
