// storebench times meta-engine operations through the hypervisor backend: update/get loop in-process at resident N, create fans out worker processes doing reserve+finalize on one shared store (§9's concurrent VM-creation shape).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	metasqlite "github.com/cocoonstack/cocoon/meta/sqlite"
	"github.com/cocoonstack/cocoon/types"
)

const opGet = "get"

var benchPayload = json.RawMessage(`{"name":"bench","state":"running","config":{"cpu":2,"memory":1073741824}}`)

type benchConfig struct{ dir string }

func (c benchConfig) BinaryName() string                  { return "bench" }
func (c benchConfig) RootDirPath() string                 { return c.dir }
func (c benchConfig) PIDFileName() string                 { return "pid" }
func (c benchConfig) TerminateGracePeriod() time.Duration { return time.Second }
func (c benchConfig) SocketWaitTimeout() time.Duration    { return time.Second }
func (c benchConfig) EffectivePoolSize() int              { return 4 }
func (c benchConfig) EnsureDirs() error                   { return nil }
func (c benchConfig) RunDir() string                      { return c.dir }
func (c benchConfig) LogDir() string                      { return c.dir }
func (c benchConfig) VMRunDir(id string) string           { return filepath.Join(c.dir, id) }
func (c benchConfig) VMLogDir(id string) string           { return filepath.Join(c.dir, id) }

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: storebench update|get <engine> <n> <ops> [dir]")
		fmt.Fprintln(os.Stderr, "       storebench create <engine> <workers> <per-worker> <resident-n> [dir]")
		os.Exit(2)
	}
	mode, engine := os.Args[1], os.Args[2]
	ctx := context.Background()
	var err error
	switch mode {
	case "update", opGet:
		err = runLoop(ctx, mode, engine, atoi(os.Args[3]), atoi(os.Args[4]), argDir(5))
	case "create":
		err = runCreate(ctx, engine, atoi(os.Args[3]), atoi(os.Args[4]), atoi(os.Args[5]), argDir(6))
	case "createworker":
		err = runWorker(ctx, engine, os.Args[3], atoi(os.Args[4]), os.Args[5])
	case "micro":
		err = runMicro(ctx, engine, os.Args[3], atoi(os.Args[4]), atoi(os.Args[5]), argDir(6))
	default:
		err = fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runLoop(ctx context.Context, mode, engine string, n, ops int, dir string) error {
	b, err := openBackend(ctx, engine, dir)
	if err != nil {
		return err
	}
	seed := make([]string, 0, n)
	for i := range n {
		seed = append(seed, fmt.Sprintf("VM%024d", i))
	}
	for _, id := range seed {
		if err := b.ReserveVM(ctx, id, &types.VMConfig{Name: id, Config: types.Config{CPU: 2, Memory: 1 << 30}}, nil, dir, dir); err != nil {
			return err
		}
	}
	if pf := os.Getenv("CPUPROFILE"); pf != "" {
		f, _ := os.Create(pf) //nolint:gosec // bench-only profile path from env
		_ = pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	start := time.Now()
	for i := range ops {
		id := seed[i%n]
		if mode == opGet {
			if _, err := b.LoadRecord(ctx, id); err != nil {
				return err
			}
			continue
		}
		if err := b.UpdateRecord(ctx, id, func(r *hypervisor.VMRecord) error {
			r.State = types.VMStateRunning
			return nil
		}); err != nil {
			return err
		}
	}
	fmt.Printf("%d\n", time.Since(start).Microseconds())
	return nil
}

func runCreate(ctx context.Context, engine string, workers, per, resident int, dir string) error {
	b, err := openBackend(ctx, engine, dir)
	if err != nil {
		return err
	}
	for i := range resident {
		id := fmt.Sprintf("SEED%022d", i)
		if rerr := b.ReserveVM(ctx, id, &types.VMConfig{Name: id, Config: types.Config{CPU: 2, Memory: 1 << 30}}, nil, dir, dir); rerr != nil {
			return rerr
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmds := make([]*exec.Cmd, workers)
	for i := range workers {
		cmds[i] = exec.Command(self, "createworker", engine, fmt.Sprintf("W%03d", i), strconv.Itoa(per), dir) //nolint:gosec
		cmds[i].Stderr = os.Stderr
	}
	start := time.Now()
	for _, c := range cmds {
		if err := c.Start(); err != nil {
			return err
		}
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			return err
		}
	}
	wall := time.Since(start)
	total := workers * per
	fmt.Printf("engine=%s workers=%d per=%d resident=%d wall_us=%d rate=%.1f/s\n",
		engine, workers, per, resident, wall.Microseconds(), float64(total)/wall.Seconds())
	return nil
}

// runWorker performs `per` creates (reserve placeholder + finalize to
// running) — the meta half of one VM creation each.
func runWorker(ctx context.Context, engine, prefix string, per int, dir string) error {
	b, err := openBackend(ctx, engine, dir)
	if err != nil {
		return err
	}
	for i := range per {
		id := fmt.Sprintf("%s%0*d", prefix, 26-len(prefix), i)
		if err := b.ReserveVM(ctx, id, &types.VMConfig{Name: id, Config: types.Config{CPU: 2, Memory: 1 << 30}}, nil, dir, dir); err != nil {
			return err
		}
		if err := b.UpdateRecord(ctx, id, func(r *hypervisor.VMRecord) error {
			r.State = types.VMStateRunning
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// runMicro times one engine primitive per durable (or relaxed) transaction:
// the §9 microbench matrix. Seeding batches 1000 rows per transaction.
func runMicro(ctx context.Context, engine, op string, n, ops int, dir string) error {
	store, err := openStore(ctx, engine, dir)
	if err != nil {
		return err
	}
	ns := hypervisor.VMNamespaceName("bench")
	seed := make([]string, 0, n)
	for i := range n {
		seed = append(seed, fmt.Sprintf("S%023d", i))
	}
	for base := 0; base < n; base += 1000 {
		hi := min(base+1000, n)
		if err := store.Update(ctx, meta.Scope{Write: ns}, meta.CommitDurable, func(w meta.Writer) error {
			for _, id := range seed[base:hi] {
				if err := w.PutRaw(ctx, ns, hypervisor.TableRecords, id, benchPayload, false); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if op == "delete" && ops > n {
		return fmt.Errorf("delete needs ops <= n (%d > %d)", ops, n)
	}
	start := time.Now()
	for i := range ops {
		var err error
		switch op {
		case "insert":
			id := fmt.Sprintf("I%023d", i)
			err = store.Update(ctx, meta.Scope{Write: ns}, meta.CommitDurable, func(w meta.Writer) error {
				return w.PutRaw(ctx, ns, hypervisor.TableRecords, id, benchPayload, false)
			})
		case "replace":
			body := uniquePayload(i)
			err = store.Update(ctx, meta.Scope{Write: ns}, meta.CommitDurable, func(w meta.Writer) error {
				return w.PutRaw(ctx, ns, hypervisor.TableRecords, seed[i%n], body, false)
			})
		case "relaxed":
			body := uniquePayload(i)
			err = store.Update(ctx, meta.Scope{Write: ns}, meta.CommitRelaxed, func(w meta.Writer) error {
				return w.PutRaw(ctx, ns, hypervisor.TableRecords, seed[i%n], body, true)
			})
		case "delete":
			err = store.Update(ctx, meta.Scope{Write: ns}, meta.CommitDurable, func(w meta.Writer) error {
				return w.DeleteRaw(ctx, ns, hypervisor.TableRecords, seed[i], false)
			})
		case opGet:
			err = store.View(ctx, []string{ns}, func(r meta.Reader) error {
				_, _, gerr := r.GetRaw(ctx, ns, hypervisor.TableRecords, seed[i%n])
				return gerr
			})
		default:
			return fmt.Errorf("unknown micro op %q", op)
		}
		if err != nil {
			return err
		}
	}
	fmt.Printf("%d\n", time.Since(start).Microseconds())
	return store.Close()
}

func openBackend(ctx context.Context, engine, dir string) (*hypervisor.Backend, error) {
	store, err := openStore(ctx, engine, dir)
	if err != nil {
		return nil, err
	}
	return hypervisor.NewBackend("bench", benchConfig{dir: dir}, nil, store)
}

func openStore(ctx context.Context, engine, dir string) (meta.Store, error) {
	ns := hypervisor.VMNamespaceName("bench")
	if engine == "sqlite" {
		decl := metasqlite.Namespace{Name: ns, Tables: []string{hypervisor.TableRecords, hypervisor.TableNames, hypervisor.TableOrphanDirs}}
		path := filepath.Join(dir, metasqlite.DBFileName)
		if _, err := os.Stat(path); err != nil { //nolint:gosec
			if err := metasqlite.Init(ctx, path, decl); err != nil {
				return nil, err
			}
		}
		return metasqlite.Open(path, decl)
	}
	return metajson.Open(metajson.Namespace{
		Name:     ns,
		FilePath: filepath.Join(dir, "vms.json"),
		LockPath: filepath.Join(dir, "vms.lock"),
		Codec: metajson.TableCodec{Specs: []metajson.TableSpec{
			{Key: "vms", Table: hypervisor.TableRecords},
			{Key: "names", Table: hypervisor.TableNames},
			{Key: "orphan_dirs", Table: hypervisor.TableOrphanDirs, Optional: true, StringList: true},
		}},
	})
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad number %q\n", s)
		os.Exit(2)
	}
	return n
}

func argDir(i int) string {
	if len(os.Args) > i {
		return os.Args[i]
	}
	dir, err := os.MkdirTemp("", "storebench-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	return dir
}

// uniquePayload defeats any identical-bytes write elision so replace ops
// measure a REAL durable commit.
func uniquePayload(i int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"name":"bench","state":"running","seq":%d}`, i))
}
