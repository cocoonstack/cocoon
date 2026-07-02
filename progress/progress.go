package progress

// Nop is a no-op tracker for callers that don't need progress reporting.
var Nop Tracker = funcTracker(func(any) {})

// Tracker receives progress events during image operations; implementations must be safe for concurrent use.
type Tracker interface {
	OnEvent(any)
}

// NewTracker wraps a typed callback as a non-generic Tracker so Images can hold it in its interface.
func NewTracker[E any](fn func(E)) Tracker {
	return funcTracker(func(v any) {
		if e, ok := v.(E); ok {
			fn(e)
		}
	})
}

type funcTracker func(any)

func (f funcTracker) OnEvent(e any) { f(e) }
