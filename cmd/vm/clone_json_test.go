package vm

import (
	"encoding/json"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestCloneResultKeepsVMFieldsAndHints(t *testing.T) {
	out, err := json.Marshal(cloneResult{VM: &types.VM{ID: "vm1"}, Hints: []string{"echo 1 > /sys/bus/pci/rescan"}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "vm1" || got["hints"] == nil {
		t.Fatalf("clone JSON = %s, want the VM fields at top level plus hints", out)
	}
}
