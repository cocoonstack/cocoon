package types

import (
	"strings"
	"testing"
)

func validConfig() VMConfig {
	return VMConfig{
		Name: "test-vm",
		Config: Config{
			CPU:     2,
			Memory:  1 << 30,  // 1 GiB
			Storage: 20 << 30, // 20 GiB
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*VMConfig)
		wantErr string // substring; empty = expect nil
	}{
		{
			name:   "valid minimal config",
			modify: func(_ *VMConfig) {},
		},
		{
			name:    "empty name",
			modify:  func(c *VMConfig) { c.Name = "" },
			wantErr: "name cannot be empty",
		},
		{
			name:    "name starts with dash",
			modify:  func(c *VMConfig) { c.Name = "-bad" },
			wantErr: "name",
		},
		{
			name:    "name too long",
			modify:  func(c *VMConfig) { c.Name = strings.Repeat("a", 64) },
			wantErr: "name",
		},
		{
			name:   "name max length",
			modify: func(c *VMConfig) { c.Name = strings.Repeat("a", 63) },
		},
		{
			name:   "name with dots and dashes",
			modify: func(c *VMConfig) { c.Name = "my.vm-01_test" },
		},
		{
			name:    "cpu zero",
			modify:  func(c *VMConfig) { c.CPU = 0 },
			wantErr: "--cpu",
		},
		{
			name:    "cpu negative",
			modify:  func(c *VMConfig) { c.CPU = -1 },
			wantErr: "--cpu",
		},
		{
			name:    "memory below 512M",
			modify:  func(c *VMConfig) { c.Memory = 256 << 20 },
			wantErr: "--memory",
		},
		{
			name:   "memory exactly 512M",
			modify: func(c *VMConfig) { c.Memory = 512 << 20 },
		},
		{
			name:    "storage below 10G",
			modify:  func(c *VMConfig) { c.Storage = 5 << 30 },
			wantErr: "--storage",
		},
		{
			name:   "storage exactly 10G",
			modify: func(c *VMConfig) { c.Storage = 10 << 30 },
		},
		{
			name:    "negative queue size",
			modify:  func(c *VMConfig) { c.QueueSize = -1 },
			wantErr: "--queue-size",
		},
		{
			name:   "zero queue size",
			modify: func(c *VMConfig) { c.QueueSize = 0 },
		},
		{
			name:    "negative disk queue size",
			modify:  func(c *VMConfig) { c.DiskQueueSize = -1 },
			wantErr: "--disk-queue-size",
		},
		{
			name:   "zero disk queue size",
			modify: func(c *VMConfig) { c.DiskQueueSize = 0 },
		},
		{
			name:   "valid username",
			modify: func(c *VMConfig) { c.User = "deploy_user" },
		},
		{
			name:    "username with uppercase",
			modify:  func(c *VMConfig) { c.User = "Admin" },
			wantErr: "--user",
		},
		{
			name:    "username starts with digit",
			modify:  func(c *VMConfig) { c.User = "1root" },
			wantErr: "--user",
		},
		{
			name:    "username too long",
			modify:  func(c *VMConfig) { c.User = strings.Repeat("a", 33) },
			wantErr: "--user",
		},
		{
			name:   "empty username is allowed",
			modify: func(c *VMConfig) { c.User = "" },
		},
		{
			name:   "safe password",
			modify: func(c *VMConfig) { c.Password = "s3cret-Pass_123" },
		},
		{
			name:    "password with backtick",
			modify:  func(c *VMConfig) { c.Password = "pass`cmd`" },
			wantErr: "--password",
		},
		{
			name:    "password with dollar",
			modify:  func(c *VMConfig) { c.Password = "pass$VAR" },
			wantErr: "--password",
		},
		{
			name:    "password with semicolon",
			modify:  func(c *VMConfig) { c.Password = "pass;rm -rf" },
			wantErr: "--password",
		},
		{
			name:    "password with pipe",
			modify:  func(c *VMConfig) { c.Password = "pass|cat" },
			wantErr: "--password",
		},
		{
			name:   "empty password is allowed",
			modify: func(c *VMConfig) { c.Password = "" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(&cfg)
			err := cfg.Validate()

			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}
