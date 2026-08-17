package config

import (
	"testing"
)

func TestValidate_OK(t *testing.T) {
	c := &Config{
		RootDir:            "/var/lib/cocoon",
		RunDir:             "/var/lib/cocoon/run",
		LogDir:             "/var/log/cocoon",
		StopTimeoutSeconds: 30,
		DNS:                "8.8.8.8,1.1.1.1",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_EmptyDNS(t *testing.T) {
	c := &Config{
		RootDir:            "/var/lib/cocoon",
		RunDir:             "/var/lib/cocoon/run",
		LogDir:             "/var/log/cocoon",
		StopTimeoutSeconds: 30,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("empty DNS should be valid: %v", err)
	}
}

func TestValidate_InvalidDNS(t *testing.T) {
	c := &Config{
		RootDir:            "/var/lib/cocoon",
		RunDir:             "/var/lib/cocoon/run",
		LogDir:             "/var/log/cocoon",
		StopTimeoutSeconds: 30,
		DNS:                "not-an-ip",
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for invalid DNS")
	}
}

func TestValidate_MissingRootDir(t *testing.T) {
	c := &Config{
		RunDir:             "/var/lib/cocoon/run",
		LogDir:             "/var/log/cocoon",
		StopTimeoutSeconds: 30,
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for empty root_dir")
	}
}

func TestValidate_BadTimeout(t *testing.T) {
	c := &Config{
		RootDir: "/var/lib/cocoon",
		RunDir:  "/var/lib/cocoon/run",
		LogDir:  "/var/log/cocoon",
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for zero stop_timeout_seconds")
	}
}

func TestDNSServers(t *testing.T) {
	tests := []struct {
		name    string
		dns     string
		want    int
		wantErr bool
	}{
		{"empty", "", 0, false},
		{"single", "8.8.8.8", 1, false},
		{"multiple comma", "8.8.8.8,1.1.1.1", 2, false},
		{"multiple semicolon", "8.8.8.8;1.1.1.1", 2, false},
		{"with spaces", " 8.8.8.8 , 1.1.1.1 ", 2, false},
		{"invalid", "not-an-ip", 0, true},
		{"mixed valid invalid", "8.8.8.8,bad", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{DNS: tt.dns}
			got, err := c.DNSServers()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tt.want {
				t.Errorf("got %d servers, want %d", len(got), tt.want)
			}
		})
	}
}

func TestValidate_NetScope(t *testing.T) {
	for _, tt := range []struct {
		scope string
		ok    bool
	}{
		{"", true}, {"mt", true}, {"m", false}, {"mtap", false}, {"m-", false}, {"bt", false}, {"rm", false},
	} {
		c := &Config{
			RootDir: "/r", RunDir: "/r/run", LogDir: "/l",
			StopTimeoutSeconds: 30, NetScope: tt.scope,
		}
		err := c.Validate()
		if tt.ok && err != nil {
			t.Fatalf("scope %q: unexpected error %v", tt.scope, err)
		}
		if !tt.ok && err == nil {
			t.Fatalf("scope %q: want rejection", tt.scope)
		}
	}
}

func TestValidate_MetaBackend(t *testing.T) {
	for _, tt := range []struct {
		backend string
		ok      bool
	}{
		{"", true}, {"json", true}, {"sqlite", true}, {"typo", false},
	} {
		c := &Config{
			RootDir: "/r", RunDir: "/r/run", LogDir: "/l",
			StopTimeoutSeconds: 30, MetaBackend: tt.backend,
		}
		err := c.Validate()
		if tt.ok && err != nil {
			t.Fatalf("backend %q: unexpected error %v", tt.backend, err)
		}
		if !tt.ok && err == nil {
			t.Fatalf("backend %q: want rejection", tt.backend)
		}
	}
}
