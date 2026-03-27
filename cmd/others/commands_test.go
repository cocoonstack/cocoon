package others

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

type mockActions struct {
	calledMethod string
	calledArgs   []string
	lastCmd      *cobra.Command
	returnErr    error
}

func (m *mockActions) GC(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "GC", args, cmd
	return m.returnErr
}

func (m *mockActions) Version(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Version", args, cmd
	return m.returnErr
}

func executeCommand(mock *mockActions, args ...string) error {
	cmds := Commands(mock)

	root := &cobra.Command{Use: "cocoon"}
	root.AddCommand(cmds...)
	root.SetArgs(args)

	return root.Execute()
}

func TestOthers_SubcommandRouting(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantMethod string
		wantArgs   []string
		wantErr    bool
	}{
		{
			name:       "gc routes to GC",
			args:       []string{"gc"},
			wantMethod: "GC",
			wantArgs:   []string{},
		},
		{
			name:       "version routes to Version",
			args:       []string{"version"},
			wantMethod: "Version",
			wantArgs:   []string{},
		},
		{
			name:    "completion missing shell arg",
			args:    []string{"completion"},
			wantErr: true,
		},
		{
			name:    "unknown subcommand errors",
			args:    []string{"bogus"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockActions{}
			err := executeCommand(mock, tt.args...)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if mock.calledMethod != tt.wantMethod {
				t.Errorf("method = %q, want %q", mock.calledMethod, tt.wantMethod)
			}

			if len(mock.calledArgs) != len(tt.wantArgs) {
				t.Fatalf("args count = %d, want %d", len(mock.calledArgs), len(tt.wantArgs))
			}

			for i, a := range tt.wantArgs {
				if mock.calledArgs[i] != a {
					t.Errorf("arg[%d] = %q, want %q", i, mock.calledArgs[i], a)
				}
			}
		})
	}
}

func TestOthers_CompletionRouting(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "bash completion", args: []string{"completion", "bash"}},
		{name: "zsh completion", args: []string{"completion", "zsh"}},
		{name: "fish completion", args: []string{"completion", "fish"}},
		{name: "powershell completion", args: []string{"completion", "powershell"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockActions{}
			err := executeCommand(mock, tt.args...)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Completion is handled by cobra directly, not by mockActions.
			if mock.calledMethod != "" {
				t.Errorf("method = %q, want empty (handled by cobra)", mock.calledMethod)
			}
		})
	}
}

func TestOthers_ErrorPropagation(t *testing.T) {
	sentinel := errors.New("handler failed")
	mock := &mockActions{returnErr: sentinel}

	err := executeCommand(mock, "gc")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "handler failed") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "handler failed")
	}
}
