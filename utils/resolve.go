package utils

// InitNamedIndex initializes nil maps in a named index (Items + Names pattern).
func InitNamedIndex[T any](items *map[string]*T, names *map[string]string) {
	if *items == nil {
		*items = make(map[string]*T)
	}
	if *names == nil {
		*names = make(map[string]string)
	}
}
