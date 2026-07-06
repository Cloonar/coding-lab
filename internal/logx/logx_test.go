package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"testing"
)

func TestNewEmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Info("hello", "component", "store", "repo", "repo_abc")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", rec["msg"])
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if rec["component"] != "store" {
		t.Errorf("component = %v, want store", rec["component"])
	}
	if rec["repo"] != "repo_abc" {
		t.Errorf("repo = %v, want repo_abc", rec["repo"])
	}
	if _, ok := rec["time"]; !ok {
		t.Error("record missing time field")
	}
}

func TestNewDropsDebugByDefault(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).Debug("nope")
	if buf.Len() != 0 {
		t.Fatalf("debug record emitted at default level: %s", buf.String())
	}
}

func TestNewRequestIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{16}$`)
	a, b := NewRequestID(), NewRequestID()
	if !re.MatchString(a) {
		t.Fatalf("NewRequestID() = %q, want match for %s", a, re)
	}
	if a == b {
		t.Fatal("two request ids collided")
	}
}

func TestContextCarrier(t *testing.T) {
	ctx := context.Background()

	if FromContext(ctx) == nil {
		t.Fatal("FromContext on empty ctx returned nil")
	}
	if got := RequestID(ctx); got != "" {
		t.Fatalf("RequestID on empty ctx = %q, want empty", got)
	}

	var buf bytes.Buffer
	l := New(&buf)
	ctx = WithLogger(ctx, l)
	ctx = WithRequestID(ctx, "deadbeefdeadbeef")

	if FromContext(ctx) != l {
		t.Error("FromContext did not return the carried logger")
	}
	if got := RequestID(ctx); got != "deadbeefdeadbeef" {
		t.Errorf("RequestID = %q, want deadbeefdeadbeef", got)
	}
}

func TestFromContextFallsBackToDefault(t *testing.T) {
	ctx := WithLogger(context.Background(), nil)
	if FromContext(ctx) != slog.Default() {
		t.Fatal("nil carried logger should fall back to slog.Default()")
	}
}
