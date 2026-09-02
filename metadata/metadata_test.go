package metadata

import (
	"bytes"
	"strings"
	"testing"
)

func TestUserData_NoBootcmd(t *testing.T) {
	cfg := &Config{
		Username: "root", Password: "test",
		Networks: []NetworkInfo{
			{IP: "10.0.0.2", Prefix: 24, MAC: "aa:bb:cc:dd:ee:f0"},
		},
	}

	var buf bytes.Buffer
	if err := userDataTmpl.Execute(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if strings.Contains(out, "bootcmd:") {
		t.Errorf("bootcmd should not appear: %s", out)
	}
	if !strings.Contains(out, "root:test") {
		t.Errorf("root password missing: %s", out)
	}
	if !strings.Contains(out, "write_files:") {
		t.Errorf("write_files missing: %s", out)
	}
	if !strings.Contains(out, "/etc/systemd/network/15-cocoon-id0.network") {
		t.Errorf("fallback .network path missing: %s", out)
	}
	if !strings.Contains(out, "MACAddress=aa:bb:cc:dd:ee:f0") {
		t.Errorf("fallback MAC match missing: %s", out)
	}
	if !strings.Contains(out, "RequiredForOnline=yes") {
		t.Errorf("primary NIC online requirement missing: %s", out)
	}
}

func TestUserData_MultiNICWriteFiles(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkInfo{
			{IP: "10.0.0.2", Prefix: 24, Gateway: "10.0.0.1", MAC: "aa:bb:cc:dd:ee:f0"},
			{IP: "10.0.1.2", Prefix: 24, MAC: "11:22:33:44:55:66"},
		},
		DNS: []string{"8.8.8.8", "1.1.1.1"},
	}

	var buf bytes.Buffer
	if err := userDataTmpl.Execute(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "/etc/systemd/network/15-cocoon-id0.network") {
		t.Errorf("id0 fallback .network missing: %s", out)
	}
	if !strings.Contains(out, "/etc/systemd/network/15-cocoon-id1.network") {
		t.Errorf("id1 fallback .network missing: %s", out)
	}
	if !strings.Contains(out, "Gateway=10.0.0.1") {
		t.Errorf("gateway missing in fallback .network: %s", out)
	}
	if !strings.Contains(out, "DNS=8.8.8.8") || !strings.Contains(out, "DNS=1.1.1.1") {
		t.Errorf("DNS missing in fallback .network: %s", out)
	}
	if !strings.Contains(out, "RequiredForOnline=yes") {
		t.Errorf("primary NIC online requirement missing: %s", out)
	}
	if !strings.Contains(out, "RequiredForOnline=no") {
		t.Errorf("secondary NIC online requirement missing: %s", out)
	}
}

func TestNetworkConfig_SingleNIC(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkInfo{
			{IP: "10.0.0.2", Prefix: 24, Gateway: "10.0.0.1", MAC: "aa:bb:cc:dd:ee:f0"},
		},
		DNS: []string{"8.8.8.8"},
	}

	var buf bytes.Buffer
	if err := networkConfigTmpl.Execute(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "version: 2") {
		t.Errorf("version missing: %s", out)
	}
	if !strings.Contains(out, `macaddress: "aa:bb:cc:dd:ee:f0"`) {
		t.Errorf("MAC match missing: %s", out)
	}
	if !strings.Contains(out, "10.0.0.2/24") {
		t.Errorf("IP/prefix missing: %s", out)
	}
	if !strings.Contains(out, "via: 10.0.0.1") {
		t.Errorf("gateway missing: %s", out)
	}
	if !strings.Contains(out, "- 8.8.8.8") {
		t.Errorf("DNS missing: %s", out)
	}
}

func TestNetworkConfig_MultiNIC(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkInfo{
			{IP: "10.0.0.2", Prefix: 16, Gateway: "10.0.0.1", MAC: "aa:bb:cc:dd:ee:f0"},
			{IP: "10.0.1.2", Prefix: 24, MAC: "11:22:33:44:55:66"},
		},
		DNS: []string{"8.8.8.8"},
	}

	var buf bytes.Buffer
	if err := networkConfigTmpl.Execute(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "id0:") {
		t.Errorf("id0 missing: %s", out)
	}
	if !strings.Contains(out, "id1:") {
		t.Errorf("id1 missing: %s", out)
	}
	if !strings.Contains(out, `macaddress: "aa:bb:cc:dd:ee:f0"`) {
		t.Errorf("first MAC missing: %s", out)
	}
	if !strings.Contains(out, `macaddress: "11:22:33:44:55:66"`) {
		t.Errorf("second MAC missing: %s", out)
	}
	if !strings.Contains(out, "10.0.0.2/16") {
		t.Errorf("first IP missing: %s", out)
	}
	if !strings.Contains(out, "10.0.1.2/24") {
		t.Errorf("second IP missing: %s", out)
	}

	if !strings.Contains(out, "via: 10.0.0.1") {
		t.Errorf("gateway missing: %s", out)
	}
}

func TestNetworkConfig_GatewayOptional(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkInfo{
			{IP: "10.0.0.2", Prefix: 24, MAC: "aa:bb:cc:dd:ee:f0"},
		},
	}

	var buf bytes.Buffer
	if err := networkConfigTmpl.Execute(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if strings.Contains(out, "routes:") {
		t.Errorf("routes should not appear without gateway: %s", out)
	}
}

func TestNetworkConfig_DNSOptional(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkInfo{
			{IP: "10.0.0.2", Prefix: 24, MAC: "aa:bb:cc:dd:ee:f0"},
		},
	}

	var buf bytes.Buffer
	if err := networkConfigTmpl.Execute(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if strings.Contains(out, "nameservers:") {
		t.Errorf("nameservers should not appear without DNS: %s", out)
	}
}

func TestGenerate_ProducesValidFAT12(t *testing.T) {
	cfg := &Config{
		InstanceID: "test-id",
		Hostname:   "test-vm",
		Username:   "root", Password: "pass",
		Networks: []NetworkInfo{
			{IP: "10.0.0.2", Prefix: 24, Gateway: "10.0.0.1", MAC: "aa:bb:cc:dd:ee:ff"},
		},
		DNS: []string{"8.8.8.8"},
	}

	var buf bytes.Buffer
	if err := Generate(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty output")
	}

	raw := buf.String()
	if !strings.Contains(raw, `macaddress: "aa:bb:cc:dd:ee:ff"`) {
		t.Error("macaddress matching not found in FAT12 image")
	}
	if !strings.Contains(raw, "10.0.0.2/24") {
		t.Error("IP not found in FAT12 image")
	}
	if !strings.Contains(raw, "/etc/systemd/network/15-cocoon-id0.network") {
		t.Error("fallback .network path not found in FAT12 image")
	}
	if !strings.Contains(raw, "MACAddress=aa:bb:cc:dd:ee:ff") {
		t.Error("fallback .network MACAddress not found in FAT12 image")
	}
}

func TestGenerate_NoNetworks(t *testing.T) {
	cfg := &Config{
		InstanceID: "test-id",
		Hostname:   "test-vm",
		Username:   "root", Password: "pass",
	}

	var buf bytes.Buffer
	if err := Generate(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty output")
	}

	raw := buf.String()
	if strings.Contains(raw, "ethernets:") {
		t.Error("network-config should not appear without networks")
	}
	if strings.Contains(raw, "write_files:") {
		t.Error("write_files should not appear without networks")
	}
}

func TestUserData_Mounts(t *testing.T) {
	cfg := &Config{
		Mounts: []MountSpec{
			{
				Device:     "/dev/disk/by-id/virtio-data1",
				MountPoint: "/mnt/data",
				FSType:     "ext4",
				Options:    "defaults,nofail",
			},
		},
	}
	var buf bytes.Buffer
	if err := userDataTmpl.Execute(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "mounts:") {
		t.Fatalf("mounts: section missing\n%s", out)
	}
	if !strings.Contains(out, "/dev/disk/by-id/virtio-data1") || !strings.Contains(out, "/mnt/data") {
		t.Errorf("mount entry missing\n%s", out)
	}
}

func TestUserData_NoMounts(t *testing.T) {
	cfg := &Config{Username: "root", Password: "x"}
	var buf bytes.Buffer
	if err := userDataTmpl.Execute(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "mounts:") {
		t.Error("mounts: should not appear without Mounts")
	}
}

func TestUserData_MountsYAMLEscape(t *testing.T) {
	cfg := &Config{
		Mounts: []MountSpec{
			{Device: "/dev/x", MountPoint: "/mnt/it's-here", FSType: "ext4", Options: "defaults"},
		},
	}
	var buf bytes.Buffer
	if err := userDataTmpl.Execute(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "/mnt/it''s-here") {
		t.Errorf("single quote not escaped: %s", buf.String())
	}
}
