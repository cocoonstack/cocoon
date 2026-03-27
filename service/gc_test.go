package service

import (
	"context"
	"testing"
)

func TestRunGC_Success(t *testing.T) {
	hyper := defaultHypervisor()
	imgs := defaultImages()
	net := defaultNetwork()
	snap := defaultSnapshot()
	svc := newTestService(hyper, imgs, net, snap)

	err := svc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
