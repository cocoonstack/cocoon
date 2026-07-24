package cloudhypervisor

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

type patchOptions struct {
	storageConfigs []*types.StorageConfig
	netTAPs        []string
	consoleSock    string
	vsockSock      string
	directBoot     bool
	diskQueueSize  int
	noDirectIO     bool
}

// patchCHConfig patches specific fields in config.json while preserving all unknown fields that CH adds internally (platform, cpus.topology, etc.).
func patchCHConfig(path string, opts *patchOptions) error {
	var raw map[string]json.RawMessage
	if err := utils.ReadJSONFile(path, &raw); err != nil {
		return err
	}

	diskCount := rawArrayLen(raw["disks"])
	if len(opts.storageConfigs) != diskCount {
		return fmt.Errorf("disk count mismatch: storageConfigs=%d, CH config=%d",
			len(opts.storageConfigs), diskCount)
	}
	if diskRaw, ok := raw["disks"]; ok {
		patched, patchErr := patchDisks(diskRaw, opts)
		if patchErr != nil {
			return fmt.Errorf("patch disks: %w", patchErr)
		}
		raw["disks"] = patched
	}

	serial, console := serialConsoleFor(opts.directBoot, opts.consoleSock)
	if err := setField(raw, "serial", serial); err != nil {
		return fmt.Errorf("patch serial: %w", err)
	}
	if err := setField(raw, "console", console); err != nil {
		return fmt.Errorf("patch console: %w", err)
	}

	if opts.vsockSock != "" {
		if vsockRaw, ok := raw["vsock"]; ok && rawObjectPresent(vsockRaw) {
			// Preserve id so CH reuses the snapshot's PCI slot instead of allocating a colliding new one.
			patched, patchErr := patchRawObject(vsockRaw, func(obj map[string]json.RawMessage) error {
				return setField(obj, "socket", opts.vsockSock)
			})
			if patchErr != nil {
				return fmt.Errorf("patch vsock: %w", patchErr)
			}
			raw["vsock"] = patched
		}
	}

	if len(opts.netTAPs) > 0 {
		if netRaw, ok := raw["net"]; ok && rawObjectPresent(netRaw) {
			patched, patchErr := patchRawArray(netRaw, len(opts.netTAPs), func(i int, elem map[string]json.RawMessage) error {
				return setField(elem, "tap", opts.netTAPs[i])
			})
			if patchErr != nil {
				return fmt.Errorf("patch nets: %w", patchErr)
			}
			raw["net"] = patched
		}
	}

	return utils.AtomicWriteJSONNoSync(path, raw)
}

func patchDisks(diskRaw json.RawMessage, opts *patchOptions) (json.RawMessage, error) {
	diskQueueSize := opts.diskQueueSize
	if diskQueueSize <= 0 {
		diskQueueSize = defaultDiskQueueSize
	}
	return patchRawArray(diskRaw, len(opts.storageConfigs), func(i int, elem map[string]json.RawMessage) error {
		sc := opts.storageConfigs[i]
		if e := setField(elem, "path", sc.Path); e != nil {
			return e
		}
		if e := setField(elem, "queue_size", diskQueueSize); e != nil {
			return e
		}
		return setField(elem, "direct", effectiveDirectIO(sc, opts.noDirectIO))
	})
}

func rawObjectPresent(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

func rawArrayLen(raw json.RawMessage) int {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return 0
	}
	return len(arr)
}

func setField(obj map[string]json.RawMessage, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal field %q: %w", key, err)
	}
	obj[key] = raw
	return nil
}

func patchRawArray(raw json.RawMessage, count int, fn func(int, map[string]json.RawMessage) error) (json.RawMessage, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("decode array: %w", err)
	}
	if len(arr) != count {
		return nil, fmt.Errorf("array length mismatch: got %d, want %d", len(arr), count)
	}
	for i := range arr {
		var elem map[string]json.RawMessage
		if err := json.Unmarshal(arr[i], &elem); err != nil {
			return nil, fmt.Errorf("decode element %d: %w", i, err)
		}
		if err := fn(i, elem); err != nil {
			return nil, err
		}
		patched, err := json.Marshal(elem)
		if err != nil {
			return nil, fmt.Errorf("marshal element %d: %w", i, err)
		}
		arr[i] = patched
	}
	return json.Marshal(arr)
}

func patchRawObject(raw json.RawMessage, fn func(map[string]json.RawMessage) error) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("decode object: %w", err)
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	if err := fn(obj); err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}
