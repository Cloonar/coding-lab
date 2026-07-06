package config

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

// genv builds a getenv func from a map; unset keys read as "".
func genv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func baseEnv() map[string]string {
	return map[string]string{"HOME": "/home/u"}
}

func TestParse(t *testing.T) {
	defaults := Config{
		Addr:            ":8080",
		StateDir:        "/home/u/.local/state/lab",
		DB:              "sqlite:/home/u/.local/state/lab/lab.db",
		MasterKeyFile:   "/home/u/.local/state/lab/master.key",
		ClaudeBin:       "claude",
		TmuxBin:         "tmux",
		GitBin:          "git",
		PrlimitBin:      "prlimit",
		MaxInstances:    6,
		SessionNofile:   16384,
		ProxyAuth:       false,
		ProxyAuthHeader: "Remote-User",
		BaseURL:         "",
	}

	with := func(mut func(*Config)) Config {
		c := defaults
		mut(&c)
		return c
	}

	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    Config
		wantErr string // substring; "" means success expected
	}{
		{
			name: "all defaults",
			want: defaults,
		},
		{
			name: "env overrides defaults",
			env: map[string]string{
				"LAB_ADDR":     ":9090",
				"LAB_BASE_URL": "https://lab.example.com",
			},
			want: with(func(c *Config) { c.Addr = ":9090"; c.BaseURL = "https://lab.example.com" }),
		},
		{
			name: "flag beats env",
			args: []string{"--addr", ":7000"},
			env:  map[string]string{"LAB_ADDR": ":9090"},
			want: with(func(c *Config) { c.Addr = ":7000" }),
		},
		{
			name: "state dir env moves db and master key defaults",
			env:  map[string]string{"LAB_STATE_DIR": "/srv/lab"},
			want: with(func(c *Config) {
				c.StateDir = "/srv/lab"
				c.DB = "sqlite:/srv/lab/lab.db"
				c.MasterKeyFile = "/srv/lab/master.key"
			}),
		},
		{
			name: "state dir flag moves db and master key defaults",
			args: []string{"--state-dir", "/var/lib/lab"},
			want: with(func(c *Config) {
				c.StateDir = "/var/lib/lab"
				c.DB = "sqlite:/var/lib/lab/lab.db"
				c.MasterKeyFile = "/var/lib/lab/master.key"
			}),
		},
		{
			name: "explicit LAB_DB survives a state-dir flag",
			args: []string{"--state-dir", "/var/lib/lab"},
			env:  map[string]string{"LAB_DB": "postgres://lab@db/lab"},
			want: with(func(c *Config) {
				c.StateDir = "/var/lib/lab"
				c.DB = "postgres://lab@db/lab"
				c.MasterKeyFile = "/var/lib/lab/master.key"
			}),
		},
		{
			name: "db flag beats LAB_DB env",
			args: []string{"--db", "sqlite:/tmp/x.db"},
			env:  map[string]string{"LAB_DB": "postgres://lab@db/lab"},
			want: with(func(c *Config) { c.DB = "sqlite:/tmp/x.db" }),
		},
		{
			name: "master key file env and flag precedence",
			args: []string{"--master-key-file", "/run/creds/master.key"},
			env:  map[string]string{"LAB_MASTER_KEY_FILE": "/elsewhere/master.key"},
			want: with(func(c *Config) { c.MasterKeyFile = "/run/creds/master.key" }),
		},
		{
			name: "binary path overrides",
			args: []string{"--claude", "/opt/claude/bin/claude", "--tmux", "/usr/bin/tmux", "--git", "/usr/bin/git", "--prlimit", "/usr/bin/prlimit"},
			want: with(func(c *Config) {
				c.ClaudeBin = "/opt/claude/bin/claude"
				c.TmuxBin = "/usr/bin/tmux"
				c.GitBin = "/usr/bin/git"
				c.PrlimitBin = "/usr/bin/prlimit"
			}),
		},
		{
			name: "caps and proxy auth",
			args: []string{"--max-instances", "3", "--session-nofile", "0", "--proxy-auth", "--proxy-auth-header", "X-Remote-User", "--trusted-proxies", "10.0.0.0/8, fd00::/8"},
			want: with(func(c *Config) {
				c.MaxInstances = 3
				c.SessionNofile = 0
				c.ProxyAuth = true
				c.ProxyAuthHeader = "X-Remote-User"
				c.TrustedProxies = []netip.Prefix{
					netip.MustParsePrefix("10.0.0.0/8"),
					netip.MustParsePrefix("fd00::/8"),
				}
			}),
		},
		{
			name:    "trusted proxies reject a bare IP",
			args:    []string{"--trusted-proxies", "10.0.0.1"},
			wantErr: "is not a CIDR",
		},
		{
			name:    "trusted proxies reject garbage",
			args:    []string{"--trusted-proxies", "not-a-cidr/8"},
			wantErr: "is not a CIDR",
		},
		{
			name:    "max instances must be positive",
			args:    []string{"--max-instances", "0"},
			wantErr: "must be at least 1",
		},
		{
			name:    "session nofile must be non-negative",
			args:    []string{"--session-nofile", "-1"},
			wantErr: "must be >= 0",
		},
		{
			name:    "proxy auth needs a header",
			args:    []string{"--proxy-auth", "--proxy-auth-header", ""},
			wantErr: "non-empty --proxy-auth-header",
		},
		{
			name:    "base url must be http(s)",
			args:    []string{"--base-url", "ftp://lab.example.com"},
			wantErr: "http(s)",
		},
		{
			name:    "base url must be absolute",
			args:    []string{"--base-url", "lab.example.com"},
			wantErr: "http(s)",
		},
		{
			name:    "empty addr rejected",
			args:    []string{"--addr", ""},
			wantErr: "--addr must not be empty",
		},
		{
			name:    "unknown flag errors",
			args:    []string{"--bogus"},
			wantErr: "bogus",
		},
		{
			name:    "positional arguments rejected",
			args:    []string{"stray"},
			wantErr: "unexpected arguments",
		},
		{
			name:    "no HOME and no state dir",
			env:     map[string]string{"HOME": ""},
			wantErr: "HOME is unset",
		},
		{
			name: "no HOME but explicit state dir works",
			args: []string{"--state-dir", "/srv/lab"},
			env:  map[string]string{"HOME": ""},
			want: with(func(c *Config) {
				c.StateDir = "/srv/lab"
				c.DB = "sqlite:/srv/lab/lab.db"
				c.MasterKeyFile = "/srv/lab/master.key"
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv()
			for k, v := range tt.env {
				env[k] = v
			}
			got, err := Parse(tt.args, genv(env))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse(%v) = %+v, want error containing %q", tt.args, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse(%v) error = %q, want it to contain %q", tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%v) unexpected error: %v", tt.args, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%v)\n got %+v\nwant %+v", tt.args, got, tt.want)
			}
		})
	}
}
