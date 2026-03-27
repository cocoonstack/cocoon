package vm

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

func (m *mockActions) Create(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Create", args, cmd
	return m.returnErr
}

func (m *mockActions) Run(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Run", args, cmd
	return m.returnErr
}

func (m *mockActions) Clone(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Clone", args, cmd
	return m.returnErr
}

func (m *mockActions) Start(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Start", args, cmd
	return m.returnErr
}

func (m *mockActions) Stop(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Stop", args, cmd
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

func (m *mockActions) Console(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Console", args, cmd
	return m.returnErr
}

func (m *mockActions) RM(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "RM", args, cmd
	return m.returnErr
}

func (m *mockActions) Restore(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Restore", args, cmd
	return m.returnErr
}

func (m *mockActions) Debug(cmd *cobra.Command, args []string) error {
	m.calledMethod, m.calledArgs, m.lastCmd = "Debug", args, cmd
	return m.returnErr
}

func executeCommand(mock *mockActions, args ...string) error {
	cmd := Command(mock)

	root := &cobra.Command{Use: "cocoon"}
	root.AddCommand(cmd)
	root.SetArgs(append([]string{"vm"}, args...))

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

func TestVM_SubcommandRouting(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantMethod string
		wantArgs   []string
		wantErr    bool
	}{
		{
			name:       "create routes to Create",
			args:       []string{"create", "ubuntu:24.04"},
			wantMethod: "Create",
			wantArgs:   []string{"ubuntu:24.04"},
		},
		{
			name:       "run routes to Run",
			args:       []string{"run", "alpine:3.19"},
			wantMethod: "Run",
			wantArgs:   []string{"alpine:3.19"},
		},
		{
			name:       "clone routes to Clone",
			args:       []string{"clone", "snap-abc123"},
			wantMethod: "Clone",
			wantArgs:   []string{"snap-abc123"},
		},
		{
			name:       "start routes to Start with multiple args",
			args:       []string{"start", "vm1", "vm2"},
			wantMethod: "Start",
			wantArgs:   []string{"vm1", "vm2"},
		},
		{
			name:       "stop routes to Stop",
			args:       []string{"stop", "vm1"},
			wantMethod: "Stop",
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
			args:       []string{"inspect", "vm1"},
			wantMethod: "Inspect",
			wantArgs:   []string{"vm1"},
		},
		{
			name:       "console routes to Console",
			args:       []string{"console", "vm1"},
			wantMethod: "Console",
			wantArgs:   []string{"vm1"},
		},
		{
			name:       "rm routes to RM with multiple args",
			args:       []string{"rm", "vm1", "vm2", "vm3"},
			wantMethod: "RM",
			wantArgs:   []string{"vm1", "vm2", "vm3"},
		},
		{
			name:       "restore routes to Restore",
			args:       []string{"restore", "vm1", "snap1"},
			wantMethod: "Restore",
			wantArgs:   []string{"vm1", "snap1"},
		},
		{
			name:       "debug routes to Debug",
			args:       []string{"debug", "ubuntu:24.04"},
			wantMethod: "Debug",
			wantArgs:   []string{"ubuntu:24.04"},
		},
		{
			name:    "create missing arg",
			args:    []string{"create"},
			wantErr: true,
		},
		{
			name:    "start missing arg",
			args:    []string{"start"},
			wantErr: true,
		},
		{
			name:    "restore missing second arg",
			args:    []string{"restore", "vm1"},
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

func TestVM_FlagParsing(t *testing.T) {
	t.Run("create with explicit flags", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock,
			"create",
			"--name", "myvm",
			"--cpu", "4",
			"--memory", "2G",
			"--storage", "20G",
			"--nics", "2",
			"--network", "mynet",
			"ubuntu:24.04",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "name", "myvm")
		assertFlag(t, mock.lastCmd, "memory", "2G")
		assertFlag(t, mock.lastCmd, "storage", "20G")
		assertFlag(t, mock.lastCmd, "network", "mynet")

		cpu, _ := mock.lastCmd.Flags().GetInt("cpu")
		if cpu != 4 {
			t.Errorf("cpu = %d, want 4", cpu)
		}

		nics, _ := mock.lastCmd.Flags().GetInt("nics")
		if nics != 2 {
			t.Errorf("nics = %d, want 2", nics)
		}
	})

	t.Run("create defaults", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "create", "alpine")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "name", "")
		assertFlag(t, mock.lastCmd, "memory", "1G")
		assertFlag(t, mock.lastCmd, "storage", "10G")
		assertFlag(t, mock.lastCmd, "network", "")

		cpu, _ := mock.lastCmd.Flags().GetInt("cpu")
		if cpu != 2 {
			t.Errorf("cpu = %d, want 2", cpu)
		}

		nics, _ := mock.lastCmd.Flags().GetInt("nics")
		if nics != 1 {
			t.Errorf("nics = %d, want 1", nics)
		}
	})

	t.Run("clone flags with inherit defaults", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "clone", "--name", "myclone", "snap1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "name", "myclone")
		assertFlag(t, mock.lastCmd, "memory", "")
		assertFlag(t, mock.lastCmd, "storage", "")
		assertFlag(t, mock.lastCmd, "network", "")

		cpu, _ := mock.lastCmd.Flags().GetInt("cpu")
		if cpu != 0 {
			t.Errorf("cpu = %d, want 0", cpu)
		}

		nics, _ := mock.lastCmd.Flags().GetInt("nics")
		if nics != 0 {
			t.Errorf("nics = %d, want 0", nics)
		}
	})

	t.Run("rm --force flag", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "rm", "--force", "vm1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		force, _ := mock.lastCmd.Flags().GetBool("force")
		if !force {
			t.Error("force = false, want true")
		}
	})

	t.Run("console --escape-char flag", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "console", "--escape-char", "^A", "vm1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "escape-char", "^A")
	})

	t.Run("console escape-char default", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "console", "vm1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "escape-char", "^]")
	})

	t.Run("restore with explicit flags", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock,
			"restore",
			"--cpu", "8",
			"--memory", "4G",
			"--storage", "50G",
			"vm1", "snap1",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		cpu, _ := mock.lastCmd.Flags().GetInt("cpu")
		if cpu != 8 {
			t.Errorf("cpu = %d, want 8", cpu)
		}

		assertFlag(t, mock.lastCmd, "memory", "4G")
		assertFlag(t, mock.lastCmd, "storage", "50G")
	})

	t.Run("debug with extra flags", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock,
			"debug",
			"--max-cpu", "16",
			"--balloon", "512",
			"--cow", "/tmp/cow.img",
			"--ch", "/usr/bin/ch",
			"ubuntu:24.04",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		maxCPU, _ := mock.lastCmd.Flags().GetInt("max-cpu")
		if maxCPU != 16 {
			t.Errorf("max-cpu = %d, want 16", maxCPU)
		}

		balloon, _ := mock.lastCmd.Flags().GetInt("balloon")
		if balloon != 512 {
			t.Errorf("balloon = %d, want 512", balloon)
		}

		assertFlag(t, mock.lastCmd, "cow", "/tmp/cow.img")
		assertFlag(t, mock.lastCmd, "ch", "/usr/bin/ch")
	})

	t.Run("debug flag defaults", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "debug", "alpine")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		maxCPU, _ := mock.lastCmd.Flags().GetInt("max-cpu")
		if maxCPU != 8 {
			t.Errorf("max-cpu = %d, want 8", maxCPU)
		}

		balloon, _ := mock.lastCmd.Flags().GetInt("balloon")
		if balloon != 0 {
			t.Errorf("balloon = %d, want 0", balloon)
		}

		assertFlag(t, mock.lastCmd, "cow", "")
		assertFlag(t, mock.lastCmd, "ch", "cloud-hypervisor")
	})

	t.Run("list format flag", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "list", "-o", "json")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "format", "json")
	})

	t.Run("list format default", func(t *testing.T) {
		mock := &mockActions{}
		err := executeCommand(mock, "list")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertFlag(t, mock.lastCmd, "format", "table")
	})
}

func TestVM_ErrorPropagation(t *testing.T) {
	sentinel := errors.New("handler failed")
	mock := &mockActions{returnErr: sentinel}

	err := executeCommand(mock, "create", "ubuntu:24.04")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "handler failed") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "handler failed")
	}
}
