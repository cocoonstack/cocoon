package images

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

func (m *mockActions) Pull(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Pull", args, cmd
	return m.returnErr
}

func (m *mockActions) Import(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Import", args, cmd
	return m.returnErr
}

func (m *mockActions) List(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "List", args, cmd
	return m.returnErr
}

func (m *mockActions) RM(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "RM", args, cmd
	return m.returnErr
}

func (m *mockActions) Inspect(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Inspect", args, cmd
	return m.returnErr
}

func executeCommand(mock *mockActions, args ...string) error {
	cmd := Command(mock)

	root := &cobra.Command{Use: "cocoon"}
	root.AddCommand(cmd)
	root.SetArgs(append([]string{"image"}, args...))

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

func TestImage_SubcommandRouting(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantMethod string
		wantArgs   []string
		wantErr    bool
	}{
		{
			name:       "pull routes to Pull",
			args:       []string{"pull", "ubuntu:24.04"},
			wantMethod: "Pull",
			wantArgs:   []string{"ubuntu:24.04"},
		},
		{
			name:       "pull multiple images",
			args:       []string{"pull", "alpine:3.19", "nginx:latest"},
			wantMethod: "Pull",
			wantArgs:   []string{"alpine:3.19", "nginx:latest"},
		},
		{
			name:       "import routes to Import",
			args:       []string{"import", "--file", "/tmp/layer.tar", "myimage"},
			wantMethod: "Import",
			wantArgs:   []string{"myimage"},
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
			name:       "rm routes to RM",
			args:       []string{"rm", "img1", "img2"},
			wantMethod: "RM",
			wantArgs:   []string{"img1", "img2"},
		},
		{
			name:       "inspect routes to Inspect",
			args:       []string{"inspect", "ubuntu:24.04"},
			wantMethod: "Inspect",
			wantArgs:   []string{"ubuntu:24.04"},
		},
		{
			name:    "pull missing arg",
			args:    []string{"pull"},
			wantErr: true,
		},
		{
			name:    "rm missing arg",
			args:    []string{"rm"},
			wantErr: true,
		},
		{
			name:    "inspect missing arg",
			args:    []string{"inspect"},
			wantErr: true,
		},
		{
			name:    "import missing --file flag",
			args:    []string{"import", "myimage"},
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

func TestImage_FlagParsing(t *testing.T) {
	t.Run("import --file flag", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "import", "--file", "/tmp/a.tar", "--file", "/tmp/b.tar", "myimg")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		files, ferr := mock.lastCmd.Flags().GetStringArray("file")
		if ferr != nil {
			t.Fatalf("flag file: %v", ferr)
		}

		if len(files) != 2 {
			t.Fatalf("file count = %d, want 2", len(files))
		}

		if files[0] != "/tmp/a.tar" || files[1] != "/tmp/b.tar" {
			t.Errorf("files = %v, want [/tmp/a.tar /tmp/b.tar]", files)
		}
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

func TestImage_ErrorPropagation(t *testing.T) {
	sentinel := errors.New("handler failed")
	mock := &mockActions{returnErr: sentinel}

	err := executeCommand(mock, "pull", "ubuntu:24.04")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "handler failed") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "handler failed")
	}
}
