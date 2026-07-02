package metadata

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"text/template"
)

const cidataLabel = "CIDATA"

var (
	tmplFuncs = template.FuncMap{
		// yamlQuote escapes single quotes for YAML single-quoted strings.
		"yamlQuote": func(s string) string {
			return strings.ReplaceAll(s, "'", "''")
		},
	}

	metaDataTmpl = template.Must(template.New("meta-data").Parse(
		"instance-id: {{.InstanceID}}\nlocal-hostname: {{.Hostname}}\n"))

	// userDataTmpl renders cloud-config; also writes systemd-networkd fallback units so clone reinit survives netplan PERM-MAC mismatch.
	userDataTmpl = template.Must(template.New("user-data").Funcs(tmplFuncs).Parse(`#cloud-config
{{- if .Password}}
chpasswd:
  expire: false
  list:
    - '{{.Username}}:{{yamlQuote .Password}}'
ssh_pwauth: true
{{- if eq .Username "root"}}
disable_root: false
{{- end}}
{{- if and .Username (ne .Username "root")}}
runcmd:
  - [sh, -c, 'id {{.Username}} >/dev/null 2>&1 || useradd -m -s /bin/bash -N {{.Username}}']
  - [usermod, -aG, sudo, '{{.Username}}']
  - [sh, -c, 'echo ''{{.Username}}:{{.Password}}'' | chpasswd']
  - [sh, -c, 'echo ''{{.Username}} ALL=(ALL) NOPASSWD:ALL'' > /etc/sudoers.d/cocoon-{{.Username}}']
{{- end}}
{{- end}}
{{- if .Mounts}}
mounts:
{{- range .Mounts}}
  - ['{{yamlQuote .Device}}', '{{yamlQuote .MountPoint}}', '{{yamlQuote .FSType}}', '{{yamlQuote .Options}}', '0', '2']
{{- end}}
{{- end}}
{{- if .Networks}}
write_files:
{{- range $i, $n := .Networks}}
  - path: /etc/systemd/network/15-cocoon-id{{$i}}.network
    owner: root:root
    permissions: '0644'
    content: |
      [Match]
      MACAddress={{$n.MAC}}

      [Network]
{{- if $n.IP}}
      Address={{$n.IP}}/{{$n.Prefix}}
{{- if $n.Gateway}}
      Gateway={{$n.Gateway}}
{{- end}}
{{- range $.DNS}}
      DNS={{.}}
{{- end}}
{{- else}}
      DHCP=ipv4
{{- end}}
{{- if eq $i 0}}
      RequiredForOnline=yes
{{- else}}
      RequiredForOnline=no
{{- end}}
{{- end}}
{{- end}}
`))

	// networkConfigTmpl renders cloud-init network-config (netplan v2 → systemd-networkd).
	// Clone reinit fallback for netplan PERM-MAC mismatch is wired in via user-data write_files.
	networkConfigTmpl = template.Must(template.New("network-config").Parse(`version: 2
ethernets:
{{- range $i, $n := .Networks}}
  id{{$i}}:
    match:
      macaddress: "{{$n.MAC}}"
{{- if $n.IP}}
    addresses:
      - {{$n.IP}}/{{$n.Prefix}}
{{- if $n.Gateway}}
    routes:
      - to: default
        via: {{$n.Gateway}}
{{- end}}
{{- if $.DNS}}
    nameservers:
      addresses:
{{- range $.DNS}}
        - {{.}}
{{- end}}
{{- end}}
{{- else}}
    dhcp4: true
{{- end}}
{{- end}}
  zfallback:
    match:
      name: "e*"
    dhcp4: true
    optional: true
`))
)

// Config holds the inputs for generating cloud-init NoCloud metadata.
type Config struct {
	InstanceID string
	Hostname   string
	Username   string
	Password   string
	Networks   []NetworkInfo
	Mounts     []MountSpec // optional fstab entries written by cloud-init
	DNS        []string    // e.g. ["8.8.8.8", "8.8.4.4"]
}

// NetworkInfo describes a single guest network interface for cloud-init.
type NetworkInfo struct {
	IP      string // e.g. "10.0.0.2"
	Prefix  int    // CIDR prefix length, e.g. 24
	Gateway string // e.g. "10.0.0.1"
	MAC     string // MAC address for match:macaddress in network-config
}

// MountSpec is one cloud-init `mounts:` row; fields render verbatim.
type MountSpec struct {
	Device     string
	MountPoint string
	FSType     string
	Options    string
}

// Generate streams a cloud-init NoCloud cidata disk image (FAT12) to w.
func Generate(w io.Writer, cfg *Config) error {
	files := make(map[string][]byte, 3) //nolint:mnd

	var buf bytes.Buffer
	if err := metaDataTmpl.Execute(&buf, cfg); err != nil {
		return fmt.Errorf("render meta-data: %w", err)
	}
	files["meta-data"] = bytes.Clone(buf.Bytes())

	buf.Reset()
	if err := userDataTmpl.Execute(&buf, cfg); err != nil {
		return fmt.Errorf("render user-data: %w", err)
	}
	files["user-data"] = bytes.Clone(buf.Bytes())

	if len(cfg.Networks) > 0 {
		buf.Reset()
		if err := networkConfigTmpl.Execute(&buf, cfg); err != nil {
			return fmt.Errorf("render network-config: %w", err)
		}
		files["network-config"] = bytes.Clone(buf.Bytes())
	}

	return CreateFAT12(w, cidataLabel, files)
}
