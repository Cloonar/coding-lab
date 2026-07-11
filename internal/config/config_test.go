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
		VAPIDKeyFile:    "/home/u/.local/state/lab/vapid.key",
		ProviderBin:     map[string]string{},
		ProviderConfig:  map[string]string{},
		TmuxBin:         "tmux",
		GitBin:          "git",
		PrlimitBin:      "prlimit",
		MaxInstances:    6,
		SessionNofile:   16384,
		ProxyAuth:       false,
		ProxyAuthHeader: "Remote-User",
		BaseURL:         "",
		AgentURL:        "",
	}

	// with copies defaults and applies mut. NOTE: ProviderBin/ProviderConfig
	// are shared map references with defaults, so a mutation must ASSIGN a
	// fresh map (c.ProviderBin = map[...]{...}), never index into the shared
	// one, or it would corrupt defaults for every later test.
	with := func(mut func(*Config)) Config {
		c := defaults
		mut(&c)
		return c
	}

	tests := []struct {
		name        string
		args        []string
		env         map[string]string
		providerIDs []string // nil → []string{"claude-code"}
		want        Config
		wantErr     string // substring; "" means success expected
	}{
		{
			name: "all defaults",
			want: defaults,
		},
		{
			name: "no provider flags or env leaves both maps non-nil and empty",
			want: with(func(c *Config) {
				c.ProviderBin = map[string]string{}
				c.ProviderConfig = map[string]string{}
			}),
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
			name: "state dir env moves db, master key, and vapid key defaults",
			env:  map[string]string{"LAB_STATE_DIR": "/srv/lab"},
			want: with(func(c *Config) {
				c.StateDir = "/srv/lab"
				c.DB = "sqlite:/srv/lab/lab.db"
				c.MasterKeyFile = "/srv/lab/master.key"
				c.VAPIDKeyFile = "/srv/lab/vapid.key"
			}),
		},
		{
			name: "state dir flag moves db, master key, and vapid key defaults",
			args: []string{"--state-dir", "/var/lib/lab"},
			want: with(func(c *Config) {
				c.StateDir = "/var/lib/lab"
				c.DB = "sqlite:/var/lib/lab/lab.db"
				c.MasterKeyFile = "/var/lib/lab/master.key"
				c.VAPIDKeyFile = "/var/lib/lab/vapid.key"
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
				c.VAPIDKeyFile = "/var/lib/lab/vapid.key"
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
			name: "vapid key file env override",
			env:  map[string]string{"LAB_VAPID_KEY_FILE": "/elsewhere/vapid.key"},
			want: with(func(c *Config) { c.VAPIDKeyFile = "/elsewhere/vapid.key" }),
		},
		{
			name: "vapid key file flag beats env",
			args: []string{"--vapid-key-file", "/run/creds/vapid.key"},
			env:  map[string]string{"LAB_VAPID_KEY_FILE": "/elsewhere/vapid.key"},
			want: with(func(c *Config) { c.VAPIDKeyFile = "/run/creds/vapid.key" }),
		},
		{
			name: "binary path overrides",
			args: []string{"--tmux", "/usr/bin/tmux", "--git", "/usr/bin/git", "--prlimit", "/usr/bin/prlimit"},
			want: with(func(c *Config) {
				c.TmuxBin = "/usr/bin/tmux"
				c.GitBin = "/usr/bin/git"
				c.PrlimitBin = "/usr/bin/prlimit"
			}),
		},

		// --- generic per-provider host overrides (issue #78) ---
		{
			name: "provider-bin flag sets the map entry",
			args: []string{"--provider-bin", "claude-code=/opt/cc/claude"},
			want: with(func(c *Config) {
				c.ProviderBin = map[string]string{"claude-code": "/opt/cc/claude"}
			}),
		},
		{
			name:        "provider-bin repeatable, two ids both land",
			args:        []string{"--provider-bin", "claude-code=/opt/cc/claude", "--provider-bin", "codex=/opt/codex/codex"},
			providerIDs: []string{"claude-code", "codex"},
			want: with(func(c *Config) {
				c.ProviderBin = map[string]string{"claude-code": "/opt/cc/claude", "codex": "/opt/codex/codex"}
			}),
		},
		{
			name:        "provider-config repeatable, two ids both land",
			args:        []string{"--provider-config", "claude-code=/etc/cc.json", "--provider-config", "codex=/etc/codex.json"},
			providerIDs: []string{"claude-code", "codex"},
			want: with(func(c *Config) {
				c.ProviderConfig = map[string]string{"claude-code": "/etc/cc.json", "codex": "/etc/codex.json"}
			}),
		},
		{
			name: "provider-bin env sets the entry with dash to underscore normalization",
			env:  map[string]string{"LAB_PROVIDER_BIN_CLAUDE_CODE": "/env/claude"},
			want: with(func(c *Config) {
				c.ProviderBin = map[string]string{"claude-code": "/env/claude"}
			}),
		},
		{
			name: "provider-config env sets the entry",
			env:  map[string]string{"LAB_PROVIDER_CONFIG_CLAUDE_CODE": "/env/cc.json"},
			want: with(func(c *Config) {
				c.ProviderConfig = map[string]string{"claude-code": "/env/cc.json"}
			}),
		},
		{
			name: "provider-bin flag beats provider-bin env",
			args: []string{"--provider-bin", "claude-code=/flag/claude"},
			env:  map[string]string{"LAB_PROVIDER_BIN_CLAUDE_CODE": "/env/claude"},
			want: with(func(c *Config) {
				c.ProviderBin = map[string]string{"claude-code": "/flag/claude"}
			}),
		},

		// --- deprecated alias shim (issue #78) ---
		{
			name: "claude alias flag populates ProviderBin",
			args: []string{"--claude", "/opt/claude/bin/claude"},
			want: with(func(c *Config) {
				c.ProviderBin = map[string]string{"claude-code": "/opt/claude/bin/claude"}
			}),
		},
		{
			name: "claude-config alias flag populates ProviderConfig",
			args: []string{"--claude-config", "/etc/lab/claude.json"},
			want: with(func(c *Config) {
				c.ProviderConfig = map[string]string{"claude-code": "/etc/lab/claude.json"}
			}),
		},
		{
			name: "LAB_CLAUDE_CONFIG alias env populates ProviderConfig",
			env:  map[string]string{"LAB_CLAUDE_CONFIG": "/elsewhere/claude.json"},
			want: with(func(c *Config) {
				c.ProviderConfig = map[string]string{"claude-code": "/elsewhere/claude.json"}
			}),
		},
		{
			name: "claude-config alias flag beats LAB_CLAUDE_CONFIG alias env",
			args: []string{"--claude-config", "/etc/lab/claude.json"},
			env:  map[string]string{"LAB_CLAUDE_CONFIG": "/elsewhere/claude.json"},
			want: with(func(c *Config) {
				c.ProviderConfig = map[string]string{"claude-code": "/etc/lab/claude.json"}
			}),
		},
		{
			name: "generic bin flag beats claude alias flag",
			args: []string{"--provider-bin", "claude-code=/gen/claude", "--claude", "/alias/claude"},
			want: with(func(c *Config) {
				c.ProviderBin = map[string]string{"claude-code": "/gen/claude"}
			}),
		},
		{
			name: "generic bin env beats claude alias flag",
			args: []string{"--claude", "/alias/claude"},
			env:  map[string]string{"LAB_PROVIDER_BIN_CLAUDE_CODE": "/genenv/claude"},
			want: with(func(c *Config) {
				c.ProviderBin = map[string]string{"claude-code": "/genenv/claude"}
			}),
		},
		{
			name: "generic config flag beats claude-config alias flag",
			args: []string{"--provider-config", "claude-code=/gen/cc.json", "--claude-config", "/alias/cc.json"},
			want: with(func(c *Config) {
				c.ProviderConfig = map[string]string{"claude-code": "/gen/cc.json"}
			}),
		},
		{
			name: "generic config env beats claude-config alias flag",
			args: []string{"--claude-config", "/alias/cc.json"},
			env:  map[string]string{"LAB_PROVIDER_CONFIG_CLAUDE_CODE": "/genenv/cc.json"},
			want: with(func(c *Config) {
				c.ProviderConfig = map[string]string{"claude-code": "/genenv/cc.json"}
			}),
		},

		// --- generic override validation ---
		{
			name:    "provider-bin unknown id errors and names the id",
			args:    []string{"--provider-bin", "bogus=/x"},
			wantErr: "bogus",
		},
		{
			name:    "provider-config unknown id errors and names the id",
			args:    []string{"--provider-config", "bogus=/x"},
			wantErr: "bogus",
		},
		{
			name:    "provider-bin without = is malformed",
			args:    []string{"--provider-bin", "claude-code"},
			wantErr: "want id=path",
		},
		{
			name:    "provider-bin empty id is malformed",
			args:    []string{"--provider-bin", "=/x"},
			wantErr: "want id=path",
		},
		{
			name:    "provider-bin empty path is malformed",
			args:    []string{"--provider-bin", "claude-code="},
			wantErr: "want id=path",
		},
		{
			name:    "provider-bin duplicate id in the same flag errors",
			args:    []string{"--provider-bin", "claude-code=/x", "--provider-bin", "claude-code=/y"},
			wantErr: "set more than once",
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
			name: "agent url env override",
			env:  map[string]string{"LAB_AGENT_URL": "http://127.0.0.1:8080"},
			want: with(func(c *Config) { c.AgentURL = "http://127.0.0.1:8080" }),
		},
		{
			name: "agent url flag beats env",
			args: []string{"--agent-url", "http://127.0.0.1:9000"},
			env:  map[string]string{"LAB_AGENT_URL": "http://127.0.0.1:8080"},
			want: with(func(c *Config) { c.AgentURL = "http://127.0.0.1:9000" }),
		},
		{
			name: "agent url is independent of base url",
			args: []string{"--base-url", "https://lab.example.com", "--agent-url", "http://127.0.0.1:8080"},
			want: with(func(c *Config) {
				c.BaseURL = "https://lab.example.com"
				c.AgentURL = "http://127.0.0.1:8080"
			}),
		},
		{
			name:    "agent url must be http(s)",
			args:    []string{"--agent-url", "ftp://127.0.0.1:8080"},
			wantErr: "http(s)",
		},
		{
			name:    "agent url must be absolute",
			args:    []string{"--agent-url", "lab-host.internal"},
			wantErr: "http(s)",
		},
		{
			// A bare host:port (scheme omitted) trips url.Parse; validateHTTPURL
			// must still surface the actionable http(s) hint, not the parser's
			// "first path segment cannot contain colon".
			name:    "agent url bare host:port hints http(s)",
			args:    []string{"--agent-url", "127.0.0.1:8080"},
			wantErr: "http(s)",
		},
		// --- seed user (issue #134) ---
		{
			name: "seed flags land in Config",
			args: []string{"--seed-user", "admin", "--seed-password", "hunter2"},
			want: with(func(c *Config) {
				c.SeedUser = "admin"
				c.SeedPassword = "hunter2"
			}),
		},
		{
			name: "seed env lands in Config",
			env:  map[string]string{"LAB_SEED_USER": "admin", "LAB_SEED_PASSWORD_FILE": "/run/secrets/seed-pw"},
			want: with(func(c *Config) {
				c.SeedUser = "admin"
				c.SeedPasswordFile = "/run/secrets/seed-pw"
			}),
		},
		{
			name: "seed-user flag beats env",
			args: []string{"--seed-user", "flag-admin", "--seed-password", "x"},
			env:  map[string]string{"LAB_SEED_USER": "env-admin"},
			want: with(func(c *Config) {
				c.SeedUser = "flag-admin"
				c.SeedPassword = "x"
			}),
		},
		{
			name: "seed-password flag beats env",
			args: []string{"--seed-user", "admin", "--seed-password", "flag-pw"},
			env:  map[string]string{"LAB_SEED_PASSWORD": "env-pw"},
			want: with(func(c *Config) {
				c.SeedUser = "admin"
				c.SeedPassword = "flag-pw"
			}),
		},
		{
			name: "seed-password-file flag beats env",
			args: []string{"--seed-user", "admin", "--seed-password-file", "/flag/pw"},
			env:  map[string]string{"LAB_SEED_PASSWORD_FILE": "/env/pw"},
			want: with(func(c *Config) {
				c.SeedUser = "admin"
				c.SeedPasswordFile = "/flag/pw"
			}),
		},
		{
			name: "all seed values unset leaves all three empty",
			want: defaults,
		},
		{
			name:    "seed user without password source errors",
			args:    []string{"--seed-user", "admin"},
			wantErr: "--seed-user and a seed password source (--seed-password-file or --seed-password) must be set together",
		},
		{
			name:    "seed password without user errors",
			args:    []string{"--seed-password", "hunter2"},
			wantErr: "--seed-user and a seed password source (--seed-password-file or --seed-password) must be set together",
		},
		{
			name:    "seed password file without user errors",
			args:    []string{"--seed-password-file", "/run/secrets/seed-pw"},
			wantErr: "--seed-user and a seed password source (--seed-password-file or --seed-password) must be set together",
		},
		{
			name: "seed user with both password sources is valid and keeps both",
			args: []string{"--seed-user", "admin", "--seed-password", "hunter2", "--seed-password-file", "/run/secrets/seed-pw"},
			want: with(func(c *Config) {
				c.SeedUser = "admin"
				c.SeedPassword = "hunter2"
				c.SeedPasswordFile = "/run/secrets/seed-pw"
			}),
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
				c.VAPIDKeyFile = "/srv/lab/vapid.key"
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv()
			for k, v := range tt.env {
				env[k] = v
			}
			ids := tt.providerIDs
			if ids == nil {
				ids = []string{"claude-code"}
			}
			got, err := Parse(tt.args, genv(env), ids)
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
