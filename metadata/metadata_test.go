package metadata

import (
	"bytes"
	"strings"
	"testing"
)

func TestUserData_NoBootcmd(t *testing.T) {
	cfg := &Config{
		RootPassword: "test",
		Networks: []NetworkInfo{
			{IP: "10.0.0.2", Prefix: 24, Mac: "aa:bb:cc:dd:ee:f0"},
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
}

func TestNetworkConfig_SingleNIC(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkInfo{
			{IP: "10.0.0.2", Prefix: 24, Gateway: "10.0.0.1", Mac: "aa:bb:cc:dd:ee:f0"},
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
			{IP: "10.0.0.2", Prefix: 16, Gateway: "10.0.0.1", Mac: "aa:bb:cc:dd:ee:f0"},
			{IP: "10.0.1.2", Prefix: 24, Mac: "11:22:33:44:55:66"},
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
	// Gateway only on first NIC.
	if !strings.Contains(out, "via: 10.0.0.1") {
		t.Errorf("gateway missing: %s", out)
	}
}

func TestNetworkConfig_GatewayOptional(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkInfo{
			{IP: "10.0.0.2", Prefix: 24, Mac: "aa:bb:cc:dd:ee:f0"},
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
			{IP: "10.0.0.2", Prefix: 24, Mac: "aa:bb:cc:dd:ee:f0"},
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
		InstanceID:   "test-id",
		Hostname:     "test-vm",
		RootPassword: "pass",
		Networks: []NetworkInfo{
			{IP: "10.0.0.2", Prefix: 24, Gateway: "10.0.0.1", Mac: "aa:bb:cc:dd:ee:ff"},
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
	// network-config should contain MAC matching.
	if !strings.Contains(raw, `macaddress: "aa:bb:cc:dd:ee:ff"`) {
		t.Error("macaddress matching not found in FAT12 image")
	}
	if !strings.Contains(raw, "10.0.0.2/24") {
		t.Error("IP not found in FAT12 image")
	}
}

func TestGenerate_NoNetworks(t *testing.T) {
	cfg := &Config{
		InstanceID:   "test-id",
		Hostname:     "test-vm",
		RootPassword: "pass",
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
}
