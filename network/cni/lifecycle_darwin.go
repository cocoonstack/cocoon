package cni

import (
	"context"
	"errors"
)

var errNotSupported = errors.New("network namespace operations are not supported on darwin")

func createNetns(_ string) error {
	return errNotSupported
}

func deleteNetns(_ context.Context, _ string) error {
	return errNotSupported
}

func setupTCRedirect(_, _, _ string, _ int, _ string) (string, error) {
	return "", errNotSupported
}

func deleteTAPInNetns(_, _ string) error {
	return errNotSupported
}

func setPortIsolated(_ string) error {
	return errNotSupported
}
