package cni

import (
	"context"
	"errors"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
)

const (
	metaNS          = "networks"
	tableRecords    = "records"
	tableTombstones = "tombstones"
)

// MetaNamespace declares the network namespace over the legacy networks.json.
func MetaNamespace(conf *config.Config) metajson.Namespace {
	cfg := &Config{Config: conf}
	return metajson.Namespace{
		Name:     metaNS,
		FilePath: cfg.IndexFile(),
		LockPath: cfg.IndexLock(),
		Codec:    netIndexCodec{},
	}
}

var netTables = []metajson.TableSpec{
	{Key: "networks", Table: tableRecords},
	{Key: "tombstones", Table: tableTombstones, Optional: true},
}

var _ metajson.Codec = netIndexCodec{}

// netIndexCodec reproduces the legacy networkIndex file byte-for-byte;
// records cross as raw messages (no per-record re-marshal).
type netIndexCodec struct{}

func (netIndexCodec) Decode(data []byte) (*metajson.Model, error) {
	m, _, err := metajson.DecodeTables(data, netTables)
	return m, err
}

func (netIndexCodec) Encode(m *metajson.Model) ([]byte, error) {
	return metajson.EncodeTables(m, netTables)
}

// netTx is the CNI view of one meta transaction: a records-only namespace
// with the vm_id secondary lookup served by scan.
type netTx struct {
	ctx  context.Context
	r    meta.Reader
	w    meta.Writer
	recs *meta.Collection[networkRecord]
}

func (t *netTx) get(id string) (*networkRecord, error) {
	rec, err := t.recs.Get(t.ctx, t.r, id)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, nil
	}
	return rec, err
}

func (t *netTx) put(id string, rec *networkRecord) error {
	return t.recs.Upsert(t.ctx, t.w, id, rec)
}

func (t *netTx) del(id string) error {
	return t.recs.Delete(t.ctx, t.w, id)
}

func (t *netTx) scan(fn func(id string, rec *networkRecord) error) error {
	return t.recs.Scan(t.ctx, t.r, fn)
}

// byVMID returns detached copies of vmID's records.
func (t *netTx) byVMID(vmID string) ([]networkRecord, error) {
	var out []networkRecord
	if err := t.scan(func(_ string, rec *networkRecord) error {
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
			if err := t.del(id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *CNI) tx(ctx context.Context, r meta.Reader, w meta.Writer) *netTx {
	return &netTx{ctx: ctx, r: r, w: w, recs: meta.NewCollection[networkRecord](c.meta, metaNS, tableRecords)}
}
