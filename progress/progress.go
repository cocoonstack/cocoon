// Package progress defines the tracker that receives image operation events.
package progress

// Nop is a no-op tracker for callers that don't need progress reporting.
var Nop Tracker = func(any) {}

// Tracker receives progress events during image operations; implementations must be safe for concurrent use.
type Tracker func(any)

func (f Tracker) OnEvent(e any) { f(e) }

// NewTracker wraps a typed callback as a non-generic Tracker so Images can hold it in its interface.
func NewTracker[E any](fn func(E)) Tracker {
	return func(v any) {
		if e, ok := v.(E); ok {
			fn(e)
		}
	}
}
