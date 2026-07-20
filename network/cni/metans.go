package cni

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
)

const (
	metaNS     = "networks"
	tableRecs  = "records"
	tableTombs = "tombstones"
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

var _ metajson.Codec = netIndexCodec{}

// netIndexCodec reproduces the legacy networkIndex file byte-for-byte.
type netIndexCodec struct{}

func (netIndexCodec) Decode(data []byte) (*metajson.Model, error) {
	m := metajson.NewModel()
	if data == nil {
		return m, nil
	}
	var idx networkIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	for _, id := range slices.Sorted(maps.Keys(idx.Networks)) {
		raw, err := json.Marshal(idx.Networks[id])
		if err != nil {
			return nil, err
		}
		m.Put(tableRecs, id, raw)
	}
	return m, nil
}

func (netIndexCodec) Encode(m *metajson.Model) ([]byte, error) {
	idx := networkIndex{}
	idx.Init()
	if err := m.Scan(tableRecs, func(id string, raw json.RawMessage) error {
		var rec networkRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return err
		}
		idx.Networks[id] = &rec
		return nil
	}); err != nil {
		return nil, err
	}
	data, err := json.Marshal(&idx)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
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
	err := t.recs.Replace(t.ctx, t.w, id, rec)
	if errors.Is(err, meta.ErrNotFound) {
		return t.recs.Insert(t.ctx, t.w, id, rec)
	}
	return err
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

// rawView reads while the GC orchestrator holds the namespace lock (legacy ReadRaw).
func (c *CNI) rawView(ctx context.Context, fn func(*netTx) error) error {
	return c.meta.RawView(ctx, metaNS, func(r meta.Reader) error {
		return fn(c.tx(ctx, r, nil))
	})
}

// lockedUpdate writes while the GC orchestrator holds the namespace lock (legacy WriteRaw).
func (c *CNI) lockedUpdate(ctx context.Context, fn func(*netTx) error) error {
	return c.meta.LockedUpdate(ctx, metaNS, func(w meta.Writer) error {
		return fn(c.tx(ctx, w, w))
	})
}

func (c *CNI) tx(ctx context.Context, r meta.Reader, w meta.Writer) *netTx {
	return &netTx{ctx: ctx, r: r, w: w, recs: meta.NewCollection[networkRecord](c.meta, metaNS, tableRecs)}
}
