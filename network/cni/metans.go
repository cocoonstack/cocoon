package cni

import (
	"context"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
)

const (
	metaNS          = "networks"
	tableRecords    = "records"
	tableTombstones = "tombstones"
)

var netTables = metajson.TableCodec{Specs: []metajson.TableSpec{
	{Key: "networks", Table: tableRecords},
	{Key: "tombstones", Table: tableTombstones, Optional: true},
}}

// MetaNamespace declares the network namespace over the legacy networks.json.
func MetaNamespace(conf *config.Config) metajson.Namespace {
	cfg := &Config{Config: conf}
	return metajson.Namespace{
		Name:     metaNS,
		FilePath: cfg.IndexFile(),
		LockPath: cfg.IndexLock(),
		Codec:    netTables,
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
	return c.meta.View(ctx, []string{metaNS}, func(r meta.Reader) error {
		return fn(c.tx(ctx, r, nil))
	})
}

func (c *CNI) update(ctx context.Context, fn func(*netTx) error) error {
	return c.meta.Update(ctx, meta.Scope{Write: metaNS}, meta.CommitDurable, func(w meta.Writer) error {
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
	return &netTx{RecordTx: meta.NewRecordTx[networkRecord](ctx, c.meta, metaNS, tableRecords, r, w)}
}
