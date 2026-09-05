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

func setupTCRedirect(_, _, _ string, _ int, _ string) (string, int, error) {
	return "", 0, errNotSupported
}

func deleteTAPInNetns(_, _ string) error {
	return errNotSupported
}

func setLinkStateInNetns(_ string, _ []string, _ bool) error {
	return errNotSupported
}

func tapPresentInNetns(_, _ string) error {
	return errNotSupported
}
