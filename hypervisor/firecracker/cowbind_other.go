//go:build !linux

package firecracker

func launchWithBinds(_ [][2]string, _ func() (int, error)) (int, error) {
	return 0, errBindSetup
}

func verifyDriveFDs(int, [][2]string) error {
	return errBindSetup
}
