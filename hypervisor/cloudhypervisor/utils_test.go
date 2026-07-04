package cloudhypervisor

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/cocoonstack/cocoon/utils"
)

func TestIsAlreadyInStateError(t *testing.T) {
	chPaused := `PUT http://localhost/api/v1/vm.pause → 500: ["Error from API","The VM could not be paused","Cannot pause VM","Failed to pause migratable component","Invalid transition: InvalidStateTransition(Paused, Paused)"]`
	chRunning := `PUT http://localhost/api/v1/vm.resume → 500: ["Error from API","Cannot resume VM","Failed","Invalid transition: InvalidStateTransition(Running, Running)"]`

	tests := []struct {
		name  string
		err   error
		state string
		want  bool
	}{
		{name: "paused paused match", err: &utils.APIError{Code: http.StatusInternalServerError, Message: chPaused}, state: "Paused", want: true},
		{name: "running running match", err: &utils.APIError{Code: http.StatusInternalServerError, Message: chRunning}, state: "Running", want: true},
		{name: "wrong state in match", err: &utils.APIError{Code: http.StatusInternalServerError, Message: chPaused}, state: "Running", want: false},
		{name: "non-500 code", err: &utils.APIError{Code: http.StatusBadRequest, Message: chPaused}, state: "Paused", want: false},
		{name: "different transition (Created→Paused)", err: &utils.APIError{Code: http.StatusInternalServerError, Message: "InvalidStateTransition(Created, Paused)"}, state: "Paused", want: false},
		{name: "non-APIError", err: errors.New("dial unix: connection refused"), state: "Paused", want: false},
		{name: "nil error", err: nil, state: "Paused", want: false},
		{name: "wrapped APIError", err: fmt.Errorf("snapshot save: %w", &utils.APIError{Code: http.StatusInternalServerError, Message: chPaused}), state: "Paused", want: true},
		{name: "empty state", err: &utils.APIError{Code: http.StatusInternalServerError, Message: chPaused}, state: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAlreadyInStateError(tt.err, tt.state); got != tt.want {
				t.Errorf("isAlreadyInStateError(%v, %q) = %v, want %v", tt.err, tt.state, got, tt.want)
			}
		})
	}
}
