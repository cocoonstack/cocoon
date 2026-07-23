package cni

import (
	"context"

	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/meta/tombstone"
)

const (
	NamespaceName = "networks"
	TableRecords  = "records"
)

// JSONNamespace is the single source of truth for this backend's json meta layout.
func (c *Config) JSONNamespace() metajson.Namespace {
	return metajson.Namespace{
		Name:     NamespaceName,
		FilePath: c.IndexFile(),
		LockPath: c.IndexLock(),
		Codec: metajson.TableCodec{Specs: []metajson.TableSpec{
			{Key: "networks", Table: TableRecords},
			{Key: tombstone.TableName, Table: tombstone.TableName, Optional: true},
		}},
	}
}

// netTx is the CNI view of one meta transaction: a records-only namespace
// with the vm_id secondary lookup served by scan.
type netTx struct {
	*meta.RecordTx[networkRecord]
}

// byVMID returns detached copies of vmID's records.
func (t *netTx) byVMID(vmID string) ([]networkRecord, error) {
	var out []networkRecord
	if err := t.Scan(func(_ string, rec *networkRecord) error {
		if rec.VMID == vmID {
			out = append(out, *rec)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *CNI) view(ctx context.Context, fn func(*netTx) error) error {
	return c.meta.View(ctx, []string{NamespaceName}, func(r meta.Reader) error {
		return fn(c.tx(ctx, r, nil))
	})
}

func (c *CNI) update(ctx context.Context, fn func(*netTx) error) error {
	return c.meta.Update(ctx, meta.Scope{Write: NamespaceName}, meta.CommitDurable, func(w meta.Writer) error {
		return fn(c.tx(ctx, w, w))
	})
}

// deleteRecords removes the given record rows in one transaction; used by
// the Add-path stale-NIC reclaim, which runs under the VM lock.
func (c *CNI) deleteRecords(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return c.update(ctx, func(t *netTx) error {
		for _, id := range ids {
			if err := t.Del(id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *CNI) tx(ctx context.Context, r meta.Reader, w meta.Writer) *netTx {
	return &netTx{RecordTx: meta.NewRecordTx[networkRecord](ctx, c.meta, NamespaceName, TableRecords, r, w)}
}
