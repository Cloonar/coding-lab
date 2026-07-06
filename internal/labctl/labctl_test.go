package labctl

import (
	"strings"
	"testing"
)

func run(t *testing.T, args []string, env map[string]string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errw strings.Builder
	code = Run(args, Env{
		Getenv:  func(k string) string { return env[k] },
		Stdout:  &out,
		Stderr:  &errw,
		Version: "1.2.3-test",
	})
	return code, out.String(), errw.String()
}

var agentEnv = map[string]string{
	"LAB_URL":   "http://127.0.0.1:8080",
	"LAB_TOKEN": "lab_run_x",
}

func TestRunCommandSurface(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStdout string // substring
		wantStderr string // substring
	}{
		{"no args", nil, nil, 2, "", "Usage"},
		{"version", []string{"--version"}, nil, 0, "labctl 1.2.3-test", ""},
		{"help", []string{"help"}, nil, 0, "Usage", ""},
		{"unknown command", []string{"bogus"}, nil, 2, "", `unknown command "bogus"`},
		{"issue no sub", []string{"issue"}, nil, 2, "", "Usage"},
		{"issue unknown sub", []string{"issue", "bogus"}, nil, 2, "", `unknown subcommand "bogus"`},

		{"issue view stub", []string{"issue", "view"}, agentEnv, 2, "", "labctl issue view: agent API lands in M5"},
		{"issue view n stub", []string{"issue", "view", "12"}, agentEnv, 2, "", "labctl issue view: agent API lands in M5"},
		{"issue view bad n", []string{"issue", "view", "twelve"}, agentEnv, 2, "", "not an integer"},
		{"issue view too many", []string{"issue", "view", "1", "2"}, agentEnv, 2, "", "too many arguments"},

		{"issue list stub", []string{"issue", "list"}, agentEnv, 2, "", "labctl issue list: agent API lands in M5"},
		{"issue list too many", []string{"issue", "list", "x"}, agentEnv, 2, "", "too many arguments"},

		{"issue comment stub", []string{"issue", "comment", "5", "hello"}, agentEnv, 2, "", "labctl issue comment: agent API lands in M5"},
		{"issue comment missing body", []string{"issue", "comment", "5"}, agentEnv, 2, "", "want <n> <body>"},
		{"issue comment bad n", []string{"issue", "comment", "five", "hi"}, agentEnv, 2, "", "not an integer"},
		{"issue comment empty body", []string{"issue", "comment", "5", ""}, agentEnv, 2, "", "body must not be empty"},

		{"pr create stub", []string{"pr", "create", "--title", "t", "--body", "Closes #5"}, agentEnv, 2, "", "labctl pr create: agent API lands in M5"},
		{"pr create missing title", []string{"pr", "create", "--body", "b"}, agentEnv, 2, "", "--title is required"},
		{"pr create missing body", []string{"pr", "create", "--title", "t"}, agentEnv, 2, "", "--body is required"},
		{"pr without create", []string{"pr"}, agentEnv, 2, "", "Usage"},

		{"missing env", []string{"issue", "view"}, nil, 2, "", "LAB_URL and LAB_TOKEN must be set"},
		{"missing token only", []string{"issue", "list"}, map[string]string{"LAB_URL": "http://x"}, 2, "", "LAB_URL and LAB_TOKEN must be set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run(t, tt.args, tt.env)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr)
			}
			if tt.wantStdout != "" && !strings.Contains(stdout, tt.wantStdout) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout, tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr, tt.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
			if tt.wantStderr == "" && stderr != "" {
				t.Fatalf("unexpected stderr output: %q", stderr)
			}
		})
	}
}
