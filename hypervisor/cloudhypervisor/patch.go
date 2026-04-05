package cloudhypervisor

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/cocoonstack/cocoon/types"
)

type patchOptions struct {
	storageConfigs []*types.StorageConfig
	consoleSock    string
	directBoot     bool
	cpu            int
	memory         int64
}

// patchCHConfig patches specific fields in config.json while preserving all
// unknown fields that CH adds internally (platform, cpus.topology, etc.).
// If chCfg and rawData are provided, the file is not re-read.
func patchCHConfig(path string, opts *patchOptions, chCfg *chVMConfig, rawData []byte) error {
	var err error
	if rawData == nil {
		rawData, err = os.ReadFile(path) //nolint:gosec
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
	}

	var raw map[string]json.RawMessage
	if e := json.Unmarshal(rawData, &raw); e != nil {
		return fmt.Errorf("decode raw %s: %w", path, e)
	}

	diskCount := rawArrayLen(raw["disks"])
	if len(opts.storageConfigs) != diskCount {
		return fmt.Errorf("disk count mismatch: storageConfigs=%d, CH config=%d",
			len(opts.storageConfigs), diskCount)
	}
	if diskRaw, ok := raw["disks"]; ok {
		patched, patchErr := patchRawArray(diskRaw, len(opts.storageConfigs), func(i int, elem map[string]json.RawMessage) error {
			return setField(elem, "path", opts.storageConfigs[i].Path)
		})
		if patchErr != nil {
			return fmt.Errorf("patch disks: %w", patchErr)
		}
		raw["disks"] = patched
	}

	if opts.directBoot {
		_ = setField(raw, "serial", &chRuntimeFile{Mode: "Off"})
		_ = setField(raw, "console", &chRuntimeFile{Mode: "Pty"})
	} else {
		_ = setField(raw, "serial", &chRuntimeFile{Mode: "Socket", Socket: opts.consoleSock})
		_ = setField(raw, "console", &chRuntimeFile{Mode: "Off"})
	}

	if opts.cpu > 0 {
		if cpuRaw, ok := raw["cpus"]; ok {
			patched, patchErr := patchRawObject(cpuRaw, func(obj map[string]json.RawMessage) error {
				return setField(obj, "boot_vcpus", opts.cpu)
			})
			if patchErr != nil {
				return fmt.Errorf("patch cpus: %w", patchErr)
			}
			raw["cpus"] = patched
		}
	}

	if opts.memory > 0 {
		if memErr := patchMemoryAndBalloon(raw, chCfg, opts.memory); memErr != nil {
			return memErr
		}
	}

	out, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal patched config: %w", err)
	}
	return os.WriteFile(path, out, 0o600) //nolint:gosec
}

func patchMemoryAndBalloon(raw map[string]json.RawMessage, chCfg *chVMConfig, memory int64) error {
	if memRaw, ok := raw["memory"]; ok {
		patched, err := patchRawObject(memRaw, func(obj map[string]json.RawMessage) error {
			return setField(obj, "size", memory)
		})
		if err != nil {
			return fmt.Errorf("patch memory: %w", err)
		}
		raw["memory"] = patched
	}
	hasBalloon := chCfg != nil && chCfg.Balloon != nil
	if !hasBalloon {
		_, hasBalloon = raw["balloon"]
	}
	if err := patchBalloonRaw(raw, hasBalloon, memory); err != nil {
		return fmt.Errorf("patch balloon: %w", err)
	}
	return nil
}

func patchBalloonRaw(raw map[string]json.RawMessage, hasBalloon bool, memory int64) error {
	if memory < minBalloonMemory {
		delete(raw, "balloon")
		return nil
	}
	newSize := memory / defaultBalloon
	if hasBalloon {
		if balloonRaw, ok := raw["balloon"]; ok {
			patched, err := patchRawObject(balloonRaw, func(obj map[string]json.RawMessage) error {
				return setField(obj, "size", newSize)
			})
			if err != nil {
				return fmt.Errorf("patch balloon size: %w", err)
			}
			raw["balloon"] = patched
			return nil
		}
	}
	return setField(raw, "balloon", &chBalloon{
		Size:              newSize,
		DeflateOnOOM:      true,
		FreePageReporting: true,
	})
}

func rawArrayLen(raw json.RawMessage) int {
	if raw == nil {
		return 0
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return 0
	}
	return len(arr)
}

// patchStateJSON patches disk path values in state.json using structured JSON
// traversal. Only string values under keys "disk_path" or "path" are replaced,
// and only when the entire value exactly matches a key in replacements.
//
// CH's vm.restore uses config.json (not state.json) to open disk files.
// Patching state.json prevents debugging confusion from stale paths.
func patchStateJSON(path string, replacements map[string]string) error {
	if len(replacements) == 0 {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var root any
	if e := json.Unmarshal(data, &root); e != nil {
		return fmt.Errorf("decode %s: %w", path, e)
	}
	root = walkAndReplace(root, "", replacements)
	out, err := json.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal patched state: %w", err)
	}
	return os.WriteFile(path, out, 0o600) //nolint:gosec
}

func isDiskPathKey(key string) bool {
	return key == "disk_path" || key == "path"
}

// walkAndReplace recursively traverses a parsed JSON value and replaces string
// values that are under a disk-path key and exactly match a replacement entry.
func walkAndReplace(v any, key string, replacements map[string]string) any {
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			val[k] = walkAndReplace(child, k, replacements)
		}
		return val
	case []any:
		for i, child := range val {
			val[i] = walkAndReplace(child, "", replacements)
		}
		return val
	case string:
		if isDiskPathKey(key) {
			if newVal, ok := replacements[val]; ok {
				return newVal
			}
		}
		return val
	default:
		return val
	}
}

// --- Raw JSON helpers ---

// setField marshals value and stores it in obj[key].
func setField(obj map[string]json.RawMessage, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal field %q: %w", key, err)
	}
	obj[key] = raw
	return nil
}

// patchRawArray unmarshals a JSON array, applies fn to each element's raw map,
// and returns the patched array. Validates array length == count.
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

// patchRawObject unmarshals a JSON object, applies fn, and returns the patched object.
func patchRawObject(raw json.RawMessage, fn func(map[string]json.RawMessage) error) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("decode object: %w", err)
	}
	if err := fn(obj); err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}
