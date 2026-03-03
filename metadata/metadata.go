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

	// userDataTmpl renders cloud-config user-data.
	// Only handles account setup — networking is done entirely via network-config.
	userDataTmpl = template.Must(template.New("user-data").Funcs(tmplFuncs).Parse(`#cloud-config
{{- if .RootPassword}}
chpasswd:
  expire: false
  list:
    - 'root:{{yamlQuote .RootPassword}}'
ssh_pwauth: true
disable_root: false
{{- end}}
`))

	// networkConfigTmpl renders cloud-init network-config (netplan v2 passthrough).
	// Uses match:macaddress so cloud-init/netplan handles everything:
	//   - cloud-init-local renders /etc/netplan/50-cloud-init.yaml
	//   - systemd-networkd configures interfaces before network-online.target
	//   - works across create, stop, start without any bootcmd
	networkConfigTmpl = template.Must(template.New("network-config").Parse(`version: 2
ethernets:
{{- range $i, $n := .Networks}}
  id{{$i}}:
    match:
      macaddress: "{{$n.Mac}}"
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
{{- end}}
`))
)

// Config holds the inputs for generating cloud-init NoCloud metadata.
type Config struct {
	InstanceID   string
	Hostname     string
	RootPassword string
	Networks     []NetworkInfo
	DNS          []string // e.g. ["8.8.8.8", "8.8.4.4"]
}

// NetworkInfo describes a single guest network interface for cloud-init.
type NetworkInfo struct {
	IP      string // e.g. "10.0.0.2"
	Prefix  int    // CIDR prefix length, e.g. 24
	Gateway string // e.g. "10.0.0.1"
	Mac     string // MAC address for match:macaddress in network-config
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
