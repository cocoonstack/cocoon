package core

import (
	"context"
	"errors"
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestVMInUseChecksBothBackends(t *testing.T) {
	readErr := errors.New("metadata unavailable")
	for _, tt := range []struct {
		name    string
		errs    []error
		want    bool
		wantErr error
	}{
		{name: "first backend", errs: []error{nil}, want: true},
		{name: "second backend", errs: []error{hypervisor.ErrNotFound, nil}, want: true},
		{name: "orphan", errs: []error{hypervisor.ErrNotFound, hypervisor.ErrNotFound}},
		{name: "read failure", errs: []error{readErr}, wantErr: readErr},
		{name: "other read error", errs: []error{hypervisor.ErrAmbiguous}, wantErr: hypervisor.ErrAmbiguous},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hypers := make([]hypervisor.Hypervisor, 0, len(tt.errs))
			for _, err := range tt.errs {
				hypers = append(hypers, &ownerHypervisor{err: err})
			}
			got, err := vmInUse(hypers)(t.Context(), "12345678")
			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Errorf("in use = %v, %v; want %v, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

type ownerHypervisor struct {
	hypervisor.Hypervisor
	err error
}

func (h *ownerHypervisor) Inspect(context.Context, string) (*types.VM, error) {
	return nil, h.err
}
