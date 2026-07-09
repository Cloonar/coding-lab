package main

import (
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/config"
)

// TestUsageDocumentsGenericProviderFlags pins the issue #78 / ADR-0034
// acceptance that `-help` documents the generic per-provider flags and marks
// the pre-#78 spellings as deprecated aliases. usage is the package-level
// const printed on flag.ErrHelp, so assert against it directly.
func TestUsageDocumentsGenericProviderFlags(t *testing.T) {
	for _, want := range []string{
		"-provider-bin",
		"-provider-config",
		"LAB_PROVIDER_BIN_<ID>",
		"LAB_PROVIDER_CONFIG_<ID>",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not document %q", want)
		}
	}
	// The deprecated aliases must appear AND be labelled deprecated.
	if !strings.Contains(usage, "-claude") || !strings.Contains(usage, "-claude-config") {
		t.Error("usage no longer mentions the -claude / -claude-config aliases")
	}
	if !strings.Contains(usage, "deprecated") {
		t.Error("usage does not mark -claude / -claude-config as deprecated")
	}
}

// TestLabURL pins the session-facing LAB_URL precedence: the dedicated agent
// URL wins over the external base URL, which wins over the loopback fallback
// derived from --addr (issue #30).
func TestLabURL(t *testing.T) {
	tests := []struct {
		name     string
		agentURL string
		baseURL  string
		addr     string
		want     string
	}{
		{
			name: "loopback fallback from addr port",
			addr: ":8080",
			want: "http://127.0.0.1:8080",
		},
		{
			name: "loopback fallback with host in addr",
			addr: "0.0.0.0:9090",
			want: "http://127.0.0.1:9090",
		},
		{
			name: "loopback fallback defaults port when addr has none",
			addr: "unparseable",
			want: "http://127.0.0.1:8080",
		},
		{
			name:    "base url beats loopback",
			baseURL: "https://lab.example.com",
			addr:    ":8080",
			want:    "https://lab.example.com",
		},
		{
			name:     "agent url beats base url",
			agentURL: "http://127.0.0.1:8080",
			baseURL:  "https://lab.example.com",
			addr:     ":8080",
			want:     "http://127.0.0.1:8080",
		},
		{
			name:     "agent url beats loopback with no base url",
			agentURL: "http://lab-host.internal:8080",
			addr:     ":8080",
			want:     "http://lab-host.internal:8080",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := labURL(config.Config{
				AgentURL: tt.agentURL,
				BaseURL:  tt.baseURL,
				Addr:     tt.addr,
			})
			if got != tt.want {
				t.Errorf("labURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
