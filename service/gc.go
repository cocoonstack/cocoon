package service

import (
	"context"

	"github.com/projecteru2/cocoon/gc"
)

// RunGC performs cross-module garbage collection.
func (s *Service) RunGC(ctx context.Context) error {
	o := gc.New()

	for _, b := range s.images {
		b.RegisterGC(o)
	}

	s.hypervisor.RegisterGC(o)

	if s.network != nil {
		s.network.RegisterGC(o)
	}

	if s.snapshot != nil {
		s.snapshot.RegisterGC(o)
	}

	return o.Run(ctx)
}
