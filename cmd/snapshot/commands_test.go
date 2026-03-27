package snapshot

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

func (m *mockActions) Save(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Save", args, cmd
	return m.returnErr
}

func (m *mockActions) List(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "List", args, cmd
	return m.returnErr
}

func (m *mockActions) Inspect(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Inspect", args, cmd
	return m.returnErr
}

func (m *mockActions) RM(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "RM", args, cmd
	return m.returnErr
}

func executeCommand(mock *mockActions, args ...string) error {
	cmd := Command(mock)

	root := &cobra.Command{Use: "cocoon"}
	root.AddCommand(cmd)
	root.SetArgs(append([]string{"snapshot"}, args...))

	return root.Execute()
}

func assertFlag(t *testing.T, cmd *cobra.Command, name, want string) {
	t.Helper()

	got, err := cmd.Flags().GetString(name)
	if err != nil {
		t.Fatalf("flag %q: %v", name, err)
	}

	if got != want {
		t.Errorf("flag %q = %q, want %q", name, got, want)
	}
}

func TestSnapshot_SubcommandRouting(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantMethod string
		wantArgs   []string
		wantErr    bool
	}{
		{
			name:       "save routes to Save",
			args:       []string{"save", "vm1"},
			wantMethod: "Save",
			wantArgs:   []string{"vm1"},
		},
		{
			name:       "list routes to List",
			args:       []string{"list"},
			wantMethod: "List",
			wantArgs:   []string{},
		},
		{
			name:       "ls alias routes to List",
			args:       []string{"ls"},
			wantMethod: "List",
			wantArgs:   []string{},
		},
		{
			name:       "inspect routes to Inspect",
			args:       []string{"inspect", "snap1"},
			wantMethod: "Inspect",
			wantArgs:   []string{"snap1"},
		},
		{
			name:       "rm routes to RM",
			args:       []string{"rm", "snap1", "snap2"},
			wantMethod: "RM",
			wantArgs:   []string{"snap1", "snap2"},
		},
		{
			name:    "save missing arg",
			args:    []string{"save"},
			wantErr: true,
		},
		{
			name:    "inspect missing arg",
			args:    []string{"inspect"},
			wantErr: true,
		},
		{
			name:    "rm missing arg",
			args:    []string{"rm"},
			wantErr: true,
		},
		{
			name:       "unknown subcommand dispatches nothing",
			args:       []string{"bogus"},
			wantMethod: "",
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

func TestSnapshot_FlagParsing(t *testing.T) {
	t.Run("save with explicit flags", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "save", "--name", "mysnap", "--description", "before upgrade", "vm1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "name", "mysnap")
		assertFlag(t, mock.lastCmd, "description", "before upgrade")
	})

	t.Run("save flag defaults", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "save", "vm1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "name", "")
		assertFlag(t, mock.lastCmd, "description", "")
	})

	t.Run("list --vm filter", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "list", "--vm", "vm1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "vm", "vm1")
	})

	t.Run("list --vm default", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "list")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "vm", "")
	})

	t.Run("list format flag explicit", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "list", "-o", "json")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "format", "json")
	})

	t.Run("list format flag default", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "list")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "format", "table")
	})
}

func TestSnapshot_ErrorPropagation(t *testing.T) {
	sentinel := errors.New("handler failed")
	mock := &mockActions{returnErr: sentinel}

	err := executeCommand(mock, "save", "vm1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "handler failed") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "handler failed")
	}
}
