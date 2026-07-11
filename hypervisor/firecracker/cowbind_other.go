//go:build !linux

package firecracker

func launchWithBinds(_ [][2]string, _ func() (int, error)) (int, error) {
	return 0, errBindSetup
}
