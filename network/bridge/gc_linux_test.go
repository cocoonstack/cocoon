//go:build linux

package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestParseTAPName(t *testing.T) {
	tests := []struct {
		tapPrefix  string
		name       string
		wantPrefix string
		wantOK     bool
	}{
		{tapPrefix: "bt", name: "bt12345678-0", wantPrefix: "12345678", wantOK: true},
		{tapPrefix: "bt", name: "bt12345678-1", wantPrefix: "12345678", wantOK: true},
		{tapPrefix: "bt", name: "btabc-3", wantPrefix: "abc", wantOK: true},
		{tapPrefix: "bt", name: "btabc-def-5", wantPrefix: "abc-def", wantOK: true},
		{tapPrefix: "mt", name: "mt12345678-0", wantPrefix: "12345678", wantOK: true},

		{tapPrefix: "bt", name: "wrong-prefix-0"},
		{tapPrefix: "bt", name: "bt"},
		{tapPrefix: "bt", name: "bt-0"},
		{tapPrefix: "bt", name: "bt12345678"},
		{tapPrefix: "bt", name: ""},
		{tapPrefix: "mt", name: "bt12345678-0"},
	}
	for _, tt := range tests {
		label := tt.tapPrefix + "/" + tt.name
		t.Run(label, func(t *testing.T) {
			gotPrefix, gotOK := parseTAPName(tt.tapPrefix, tt.name)
			if gotOK != tt.wantOK {
				t.Errorf("parseTAPName(%q, %q) ok = %v, want %v", tt.tapPrefix, tt.name, gotOK, tt.wantOK)
			}
			if gotPrefix != tt.wantPrefix {
				t.Errorf("parseTAPName(%q, %q) prefix = %q, want %q", tt.tapPrefix, tt.name, gotPrefix, tt.wantPrefix)
			}
		})
	}
}

func TestGCRechecksTAPOwnerAfterDiscovery(t *testing.T) {
	readErr := errors.New("owner read failed")
	for _, tt := range []struct {
		name  string
		inUse bool
		err   error
	}{
		{name: "completed create", inUse: true},
		{name: "orphan"},
		{name: "read failure", err: readErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldList, oldDelete := listLinksFn, deleteLinkFn
			t.Cleanup(func() { listLinksFn, deleteLinkFn = oldList, oldDelete })
			lists, deletes, reads := 0, 0, 0
			listLinksFn = func() ([]netlink.Link, error) {
				lists++
				return []netlink.Link{&netlink.Dummy{Name: "bt12345678-0", Index: 123}}, nil
			}
			deleteLinkFn = func(link netlink.Link) error {
				deletes++
				if link.Attrs().Index != 123 {
					t.Errorf("delete index = %d", link.Attrs().Index)
				}
				return nil
			}
			m := GCModule("bt", func(_ context.Context, id string) (bool, error) {
				reads++
				if lists != 2 || id != "12345678" {
					t.Fatalf("owner read before current TAP discovery: lists=%d ref=%q", lists, id)
				}
				return tt.inUse, tt.err
			})
			snap, err := m.ReadDB(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			ids := m.Resolve(t.Context(), snap, nil)
			if err := m.Collect(t.Context(), ids, snap); !errors.Is(err, tt.err) {
				t.Fatalf("collect error = %v, want %v", err, tt.err)
			}
			if reads != 1 {
				t.Errorf("owner reads = %d, want 1", reads)
			}
			wantDeletes := 0
			if !tt.inUse && tt.err == nil {
				wantDeletes = 1
			}
			if deletes != wantDeletes {
				t.Errorf("TAP deletes = %d, want %d", deletes, wantDeletes)
			}
		})
	}
}
