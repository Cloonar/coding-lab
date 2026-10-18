package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A trimmed real /proc/self/status around the capability lines.
const statusWithAmbient = `Name:	lab
Umask:	0077
State:	S (sleeping)
Uid:	994	994	994	994
CapInh:	0000000000000400
CapPrm:	0000000000000400
CapEff:	0000000000000400
CapBnd:	000001ffffffffff
CapAmb:	0000000000000400
NoNewPrivs:	0
`

func TestParseCapAmb(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  string
		want    uint64
		wantErr bool
	}{
		{"net_bind_service", statusWithAmbient, 1 << 10, false},
		{"empty set", strings.Replace(statusWithAmbient, "CapAmb:\t0000000000000400", "CapAmb:\t0000000000000000", 1), 0, false},
		{"full set", "CapAmb:\t000001ffffffffff\n", 0x1ffffffffff, false},
		// Kernels before 4.3 have no ambient set and no line for it.
		{"no line", "Name:\tlab\nCapEff:\t0000000000000000\n", 0, false},
		{"no trailing newline", "CapAmb:\t0000000000000401", 0x401, false},
		{"garbage", "CapAmb:\tnot-hex\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCapAmb(tc.status)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("mask = %#x, want %#x", got, tc.want)
			}
		})
	}
}

// The test binary runs without ambient capabilities (no CI or dev shell
// grants any), so the real clear path must be a silent no-op: no syscall,
// nothing logged — which is also every lab started outside the NixOS
// module's preview-on-a-low-port configuration.
func TestDropAmbientCapabilitiesNoopWithoutAmbient(t *testing.T) {
	cleared, err := clearAmbientCaps()
	if err != nil {
		t.Fatalf("clearAmbientCaps: %v", err)
	}
	if cleared != 0 {
		t.Skipf("test process holds ambient capabilities %#x; the no-op case cannot be observed here", cleared)
	}
	var buf bytes.Buffer
	dropAmbientCapabilities(slog.New(slog.NewTextHandler(&buf, nil)))
	if buf.Len() != 0 {
		t.Fatalf("logged without an ambient set: %s", buf.String())
	}
}
