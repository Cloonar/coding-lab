package main

import (
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/config"
)

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
