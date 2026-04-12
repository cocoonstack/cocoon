package progress

// Tracker receives progress events during image operations.
// Implementations must be safe for concurrent use from multiple goroutines.
type Tracker interface {
	OnEvent(any)
}

// NewTracker creates a Tracker from a typed callback function.
// The caller works with a concrete event type; the Tracker interface
// stays non-generic so it can be used in interfaces like Images.
func NewTracker[E any](fn func(E)) Tracker {
	return funcTracker(func(v any) {
		if e, ok := v.(E); ok {
			fn(e)
		}
	})
}

type funcTracker func(any)

// OnEvent dispatches a progress event to the wrapped callback.
func (f funcTracker) OnEvent(e any) { f(e) }

// Nop is a no-op tracker for callers that don't need progress.
var Nop Tracker = funcTracker(func(any) {})
