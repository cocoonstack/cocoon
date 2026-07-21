package daemon

import (
	"sync"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

const (
	changeAdded    changeKind = "ADDED"
	changeModified changeKind = "MODIFIED"
	changeDeleted  changeKind = "DELETED"

	// subQueue bounds one subscriber's backlog; the stream is a hint, so a slow reader loses events rather than stalling the pass.
	subQueue = 128
)

type changeKind string

// VMStatus is one supervised VM as of the last reconcile pass.
type VMStatus struct {
	Backend              string                 `json:"backend"`
	ID                   string                 `json:"id"`
	Name                 string                 `json:"name,omitempty"`
	State                types.VMState          `json:"state"`
	TransitionGeneration uint64                 `json:"transition_generation"`
	LastTransitionReason types.TransitionReason `json:"last_transition_reason,omitempty"`
	LastTransitionAt     *time.Time             `json:"last_transition_at,omitempty"`
	Live                 bool                   `json:"live"`
	WatchedPID           int                    `json:"watched_pid,omitempty"`
	QuiescePending       bool                   `json:"quiesce_pending,omitempty"`
	VerifiedAt           time.Time              `json:"verified_at"`
}

func newVMStatus(backend string, rec *hypervisor.VMRecord, live bool, watchedPID int, at time.Time) VMStatus {
	return VMStatus{
		Backend:              backend,
		ID:                   rec.ID,
		Name:                 rec.Config.Name,
		State:                rec.State,
		TransitionGeneration: rec.TransitionGeneration,
		LastTransitionReason: rec.LastTransitionReason,
		LastTransitionAt:     rec.LastTransitionAt,
		Live:                 live,
		WatchedPID:           watchedPID,
		QuiescePending:       rec.QuiescePending,
		VerifiedAt:           at,
	}
}

// changed reports whether a consumer would act differently on the two views.
func (s VMStatus) changed(other VMStatus) bool {
	return s.State != other.State ||
		s.TransitionGeneration != other.TransitionGeneration ||
		s.LastTransitionReason != other.LastTransitionReason ||
		s.Live != other.Live ||
		s.QuiescePending != other.QuiescePending
}

type changeEvent struct {
	Kind   changeKind `json:"type"`
	Status VMStatus   `json:"vm"`
}

// cache holds the last published pass and fans its diff out to stream subscribers.
type cache struct {
	mu       sync.RWMutex
	byKey    map[watchKey]VMStatus
	order    []VMStatus
	healthy  bool
	lastPass time.Time

	nextSub int
	subs    map[int]chan changeEvent
}

func newCache() *cache {
	return &cache{byKey: map[watchKey]VMStatus{}, subs: map[int]chan changeEvent{}}
}

func (c *cache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.order)
}

// publish replaces the snapshot and emits the diff against the previous pass.
func (c *cache) publish(all []VMStatus, healthy bool, at time.Time) {
	next := make(map[watchKey]VMStatus, len(all))
	for _, st := range all {
		next[watchKey{backend: st.Backend, vmID: st.ID}] = st
	}

	c.mu.Lock()
	var events []changeEvent
	for key, st := range next {
		prev, ok := c.byKey[key]
		switch {
		case !ok:
			events = append(events, changeEvent{Kind: changeAdded, Status: st})
		case st.changed(prev):
			events = append(events, changeEvent{Kind: changeModified, Status: st})
		}
	}
	for key, prev := range c.byKey {
		if _, ok := next[key]; !ok {
			events = append(events, changeEvent{Kind: changeDeleted, Status: prev})
		}
	}
	c.byKey, c.order = next, all
	c.healthy, c.lastPass = healthy, at
	subs := make([]chan changeEvent, 0, len(c.subs))
	for _, ch := range c.subs {
		subs = append(subs, ch)
	}
	c.mu.Unlock()

	for _, ev := range events {
		for _, ch := range subs {
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

func (c *cache) snapshot() ([]VMStatus, bool, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.order, c.healthy, c.lastPass
}

// subscribe returns the change stream plus its release; the caller must drain it or lose events.
func (c *cache) subscribe() (<-chan changeEvent, func()) {
	ch := make(chan changeEvent, subQueue)
	c.mu.Lock()
	id := c.nextSub
	c.nextSub++
	c.subs[id] = ch
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.subs, id)
	}
}
