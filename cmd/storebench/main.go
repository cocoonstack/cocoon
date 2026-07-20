// storebench times single-record updates through the META json engine at
// resident N; paired against the legacy build for the P0 non-regression gate.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/types"
)

func main() {
	n, _ := strconv.Atoi(os.Args[1])
	ops, _ := strconv.Atoi(os.Args[2])
	dir, _ := os.MkdirTemp("", "storebench-*")
	defer os.RemoveAll(dir) //nolint:errcheck
	ctx := context.Background()
	store, err := metajson.Open(metajson.Namespace{
		Name:     hypervisor.VMNamespaceName("bench"),
		FilePath: filepath.Join(dir, "vms.json"),
		LockPath: filepath.Join(dir, "vms.lock"),
		Codec: metajson.TableCodec{Specs: []metajson.TableSpec{
			{Key: "vms", Table: hypervisor.TableRecords},
			{Key: "names", Table: hypervisor.TableNames},
		}},
	})
	if err != nil {
		panic(err)
	}
	b, err := hypervisor.NewBackend("bench", benchConfig{dir: dir}, nil, store)
	if err != nil {
		panic(err)
	}
	seed := make([]string, 0, n)
	for i := range n {
		seed = append(seed, fmt.Sprintf("VM%024d", i))
	}
	for _, id := range seed {
		if err := b.ReserveVM(ctx, id, &types.VMConfig{Name: id, Config: types.Config{CPU: 2, Memory: 1 << 30}}, nil, dir, dir); err != nil {
			panic(err)
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
		if err := b.UpdateRecord(ctx, id, func(r *hypervisor.VMRecord) error {
			r.State = types.VMStateRunning
			return nil
		}); err != nil {
			panic(err)
		}
	}
	fmt.Printf("%d\n", time.Since(start).Microseconds())
}

type benchConfig struct{ dir string }

func (c benchConfig) BinaryName() string                  { return "bench" }
func (c benchConfig) RootDirPath() string                 { return c.dir }
func (c benchConfig) PIDFileName() string                 { return "pid" }
func (c benchConfig) TerminateGracePeriod() time.Duration { return time.Second }
func (c benchConfig) SocketWaitTimeout() time.Duration    { return time.Second }
func (c benchConfig) EffectivePoolSize() int              { return 4 }
func (c benchConfig) IndexFile() string                   { return filepath.Join(c.dir, "vms.json") }
func (c benchConfig) IndexLock() string                   { return filepath.Join(c.dir, "vms.lock") }
func (c benchConfig) EnsureDirs() error                   { return nil }
func (c benchConfig) RunDir() string                      { return c.dir }
func (c benchConfig) LogDir() string                      { return c.dir }
func (c benchConfig) VMRunDir(id string) string           { return filepath.Join(c.dir, id) }
func (c benchConfig) VMLogDir(id string) string           { return filepath.Join(c.dir, id) }
