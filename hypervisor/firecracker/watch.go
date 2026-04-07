package firecracker

// WatchPath returns the path to the VM index file for filesystem-based
// change watching. Implements hypervisor.Watchable.
func (fc *Firecracker) WatchPath() string {
	return fc.conf.IndexFile()
}
