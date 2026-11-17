//go:build linux

package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestGCKeepsCollectingPastAnOwnerLookupFailure(t *testing.T) {
	swap(t, &listLinksFn, func() ([]netlink.Link, error) {
		return []netlink.Link{&netlink.Dummy{Name: "btAAAAAAAA-0", Index: 1}, &netlink.Dummy{Name: "btBBBBBBBB-0", Index: 2}}, nil
	})
	var deleted []int
	swap(t, &deleteLinkFn, func(link netlink.Link) error {
		deleted = append(deleted, link.Attrs().Index)
		return nil
	})
	readErr := errors.New("owner read failed")
	m := GCModule("bt", func(_ context.Context, id string) (bool, error) {
		if id == "AAAAAAAA" {
			return false, readErr
		}
		return false, nil
	})
	err := m.Collect(t.Context(), []string{"AAAAAAAA", "BBBBBBBB"}, bridgeSnapshot{})
	if !errors.Is(err, readErr) {
		t.Fatalf("collect error = %v, want the owner read failure reported", err)
	}
	if len(deleted) != 1 || deleted[0] != 2 {
		t.Fatalf("deleted = %v, want only the orphan whose owner lookup succeeded", deleted)
	}
}

func TestGCReportsAFailedTAPDelete(t *testing.T) {
	swap(t, &listLinksFn, func() ([]netlink.Link, error) {
		return []netlink.Link{&netlink.Dummy{Name: "btAAAAAAAA-0", Index: 1}, &netlink.Dummy{Name: "btAAAAAAAA-1", Index: 2}}, nil
	})
	delErr := errors.New("link busy")
	var deleted []int
	swap(t, &deleteLinkFn, func(link netlink.Link) error {
		deleted = append(deleted, link.Attrs().Index)
		if link.Attrs().Index == 1 {
			return delErr
		}
		return nil
	})
	m := GCModule("bt", func(context.Context, string) (bool, error) { return false, nil })
	err := m.Collect(t.Context(), []string{"AAAAAAAA"}, bridgeSnapshot{})
	if !errors.Is(err, delErr) {
		t.Fatalf("collect error = %v, want the TAP delete failure reported", err)
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted = %v, want every orphan TAP attempted past the failure", deleted)
	}
}

func TestGCReadsAnInterruptedLinkDump(t *testing.T) {
	swap(t, &listLinksFn, func() ([]netlink.Link, error) {
		return []netlink.Link{&netlink.Dummy{Name: "btAAAAAAAA-0", Index: 1}}, netlink.ErrDumpInterrupted
	})
	snap, err := GCModule("bt", nil).ReadDB(t.Context())
	if err != nil {
		t.Fatalf("ReadDB: %v, want an interrupted dump read as its partial list", err)
	}
	if _, ok := snap.prefixes["AAAAAAAA"]; !ok {
		t.Errorf("prefixes = %v, want the TAP from the partial list", snap.prefixes)
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
			lists, deletes, reads := 0, 0, 0
			swap(t, &listLinksFn, func() ([]netlink.Link, error) {
				lists++
				return []netlink.Link{&netlink.Dummy{Name: "btAB234567-0", Index: 123}}, nil
			})
			swap(t, &deleteLinkFn, func(link netlink.Link) error {
				deletes++
				if link.Attrs().Index != 123 {
					t.Errorf("delete index = %d", link.Attrs().Index)
				}
				return nil
			})
			m := GCModule("bt", func(_ context.Context, id string) (bool, error) {
				reads++
				if lists != 2 || id != "AB234567" {
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

func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}
