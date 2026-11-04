package metadata

import (
	"bytes"
	"encoding/binary"
	"maps"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestUserData_NonRootPasswordSetOnlyByRuncmd(t *testing.T) {
	cfg := &Config{Username: "admin", Password: "pw"}

	var buf bytes.Buffer
	if err := userDataTmpl.Execute(&buf, cfg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if strings.Contains(out, "chpasswd:") {
		t.Errorf("chpasswd module would run before runcmd creates admin: %s", out)
	}
	if !strings.Contains(out, "ssh_pwauth: true") || !strings.Contains(out, "useradd -m -s /bin/bash -N admin") || !strings.Contains(out, "echo ''admin:pw'' | chpasswd") {
		t.Errorf("runcmd must create admin and set its password: %s", out)
	}
}

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
	if strings.Count(out, "RequiredForOnline=") != 2 ||
		!strings.Contains(out, "[Link]\n      RequiredForOnline=yes") || !strings.Contains(out, "[Link]\n      RequiredForOnline=no") {
		t.Errorf("RequiredForOnline must sit directly under [Link], once per NIC: %s", out)
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
	if strings.Count(out, "optional: true") != 2 || !strings.Contains(out, "id1:\n    match:\n      macaddress: \"11:22:33:44:55:66\"\n    optional: true") {
		t.Errorf("only the secondary NIC and the fallback may be optional: %s", out)
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
	label, files := readFAT12(t, buf.Bytes())

	if label != "CIDATA" {
		t.Errorf("volume label = %q, want CIDATA", label)
	}
	if got := slices.Sorted(maps.Keys(files)); !slices.Equal(got, []string{"meta-data", "network-config", "user-data"}) {
		t.Fatalf("files in the image = %v", got)
	}
	if !strings.Contains(string(files["meta-data"]), "test-id") {
		t.Errorf("meta-data lacks the instance id: %s", files["meta-data"])
	}
	network := string(files["network-config"])
	if !strings.Contains(network, `macaddress: "aa:bb:cc:dd:ee:ff"`) || !strings.Contains(network, "10.0.0.2/24") {
		t.Errorf("network-config content wrong: %s", network)
	}
	userData := string(files["user-data"])
	if !strings.Contains(userData, "/etc/systemd/network/15-cocoon-id0.network") || !strings.Contains(userData, "MACAddress=aa:bb:cc:dd:ee:ff") {
		t.Errorf("user-data fallback .network missing: %s", userData)
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
	_, files := readFAT12(t, buf.Bytes())

	if got := slices.Sorted(maps.Keys(files)); !slices.Equal(got, []string{"meta-data", "user-data"}) {
		t.Fatalf("files in the image = %v, want no network-config", got)
	}
	if strings.Contains(string(files["user-data"]), "write_files:") {
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

func readFAT12(t *testing.T, img []byte) (string, map[string][]byte) {
	t.Helper()
	if len(img) < 512 || img[510] != 0x55 || img[511] != 0xAA {
		t.Fatal("boot sector signature 0x55AA missing")
	}
	if got := string(img[54:62]); got != "FAT12   " {
		t.Fatalf("filesystem type = %q, want FAT12", got)
	}
	bytesPerSector := int(binary.LittleEndian.Uint16(img[11:]))
	clusterBytes := int(img[13]) * bytesPerSector
	fatOff := int(binary.LittleEndian.Uint16(img[14:])) * bytesPerSector
	rootOff := fatOff + int(img[16])*int(binary.LittleEndian.Uint16(img[22:]))*bytesPerSector
	dataOff := rootOff + int(binary.LittleEndian.Uint16(img[17:]))*32
	if want := int(binary.LittleEndian.Uint16(img[19:])) * bytesPerSector; len(img) != want {
		t.Fatalf("image is %d bytes, boot sector says %d", len(img), want)
	}
	next := func(cluster int) int {
		off := fatOff + cluster + cluster/2
		word := int(img[off]) | int(img[off+1])<<8
		if cluster%2 == 0 {
			return word & 0xFFF
		}
		return word >> 4
	}

	label := strings.TrimRight(string(img[43:54]), " ")
	files := map[string][]byte{}
	var parts [][]uint16
	for off := rootOff; off < dataOff && img[off] != 0; off += 32 {
		entry := img[off : off+32]
		switch entry[11] {
		case 0x0F:
			var chars []uint16
			for _, span := range [][2]int{{1, 11}, {14, 26}, {28, 32}} {
				for i := span[0]; i < span[1]; i += 2 {
					chars = append(chars, binary.LittleEndian.Uint16(entry[i:]))
				}
			}
			parts = append(parts, chars)
		case 0x08:
		default:
			var chars []uint16
			for _, part := range slices.Backward(parts) {
				chars = append(chars, part...)
			}
			parts = nil
			if i := slices.Index(chars, 0); i >= 0 {
				chars = chars[:i]
			}
			name := string(utf16.Decode(chars))
			size := int(binary.LittleEndian.Uint32(entry[28:]))
			var data []byte
			cluster := int(binary.LittleEndian.Uint16(entry[26:]))
			for hops := 0; len(data) < size; hops++ {
				if cluster < 2 || cluster >= 0xFF8 || hops > len(img) {
					t.Fatalf("%s: broken FAT chain at cluster %d after %d bytes of %d", name, cluster, len(data), size)
				}
				start := dataOff + (cluster-2)*clusterBytes
				data = append(data, img[start:start+clusterBytes]...)
				cluster = next(cluster)
			}
			if size > 0 && cluster < 0xFF8 {
				t.Fatalf("%s: FAT chain continues past %d bytes", name, size)
			}
			files[name] = data[:size]
		}
	}
	return label, files
}
