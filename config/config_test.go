package config

import (
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"ok with dns", func(c *Config) { c.DNS = "8.8.8.8,1.1.1.1" }, false},
		{"empty dns", func(*Config) {}, false},
		{"invalid dns", func(c *Config) { c.DNS = "not-an-ip" }, true},
		{"missing root_dir", func(c *Config) { c.RootDir = "" }, true},
		{"bad timeout", func(c *Config) { c.StopTimeoutSeconds = 0 }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(&c)
			if err := c.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
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
		{"", true}, {"mt", true}, {"A9", true}, {"m", false}, {"mtap", false}, {"m-", false}, {"m/", false}, {"bt", false}, {"rm", false},
	} {
		c := validConfig()
		c.NetScope = tt.scope
		err := c.Validate()
		if tt.ok && err != nil {
			t.Fatalf("scope %q: unexpected error %v", tt.scope, err)
		}
		if !tt.ok && err == nil {
			t.Fatalf("scope %q: want rejection", tt.scope)
		}
	}
}

func TestScopeFamilies(t *testing.T) {
	for _, tt := range []struct {
		scope   string
		wantTAP string
		wantNS  string
	}{
		{"", "bt", "cocoon-"},
		{"mt", "mt", "mt-"},
	} {
		c := &Config{NetScope: tt.scope}
		if got := c.BridgeTAPPrefix(); got != tt.wantTAP {
			t.Errorf("BridgeTAPPrefix(%q) = %q, want %q", tt.scope, got, tt.wantTAP)
		}
		if got := c.NetnsPrefix(); got != tt.wantNS {
			t.Errorf("NetnsPrefix(%q) = %q, want %q", tt.scope, got, tt.wantNS)
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
		c := validConfig()
		c.MetaBackend = tt.backend
		err := c.Validate()
		if tt.ok && err != nil {
			t.Fatalf("backend %q: unexpected error %v", tt.backend, err)
		}
		if !tt.ok && err == nil {
			t.Fatalf("backend %q: want rejection", tt.backend)
		}
	}
}

func validConfig() Config {
	return Config{
		RootDir:            "/var/lib/cocoon",
		RunDir:             "/var/lib/cocoon/run",
		LogDir:             "/var/log/cocoon",
		StopTimeoutSeconds: 30,
	}
}
