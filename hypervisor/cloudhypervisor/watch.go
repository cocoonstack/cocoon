package cloudhypervisor

// WatchPath returns the path to the VM index file for filesystem-based
// change watching. Implements hypervisor.Watchable.
func (ch *CloudHypervisor) WatchPath() string {
	return ch.conf.IndexFile()
}
