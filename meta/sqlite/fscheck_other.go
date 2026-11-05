//go:build !linux

package sqlite

// statfsCheck is enforced on Linux; other platforms are dev-only.
func statfsCheck(string) error { return nil }
