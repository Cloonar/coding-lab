package events

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPublishReachesAllSubscribers(t *testing.T) {
	b := NewBus()
	ctx := context.Background()

	ch1, cancel1 := b.Subscribe(ctx)
	defer cancel1()
	ch2, cancel2 := b.Subscribe(ctx)
	defer cancel2()

	b.Publish(Event{Type: "repo.changed"})
	b.Publish(Event{Type: "run.changed"})

	for name, ch := range map[string]<-chan Event{"ch1": ch1, "ch2": ch2} {
		for _, want := range []string{"repo.changed", "run.changed"} {
			select {
			case got := <-ch:
				if got.Type != want {
					t.Fatalf("%s: got event %q, want %q", name, got.Type, want)
				}
			case <-time.After(time.Second):
				t.Fatalf("%s: timed out waiting for %q", name, want)
			}
		}
	}
}

func TestSlowSubscriberDropsInsteadOfBlocking(t *testing.T) {
	b := NewBus()
	_, cancelSlow := b.Subscribe(context.Background())
	defer cancelSlow()
	fast, cancelFast := b.Subscribe(context.Background())
	defer cancelFast()

	// The slow subscriber never reads. Well past its buffer, Publish must
	// still return promptly (the test would hang forever otherwise).
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < subscriberBuffer*4; i++ {
			b.Publish(Event{Type: "clone.progress"})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}

	// The fast subscriber kept only its buffer's worth.
	n := 0
	for {
		select {
		case <-fast:
			n++
			continue
		default:
		}
		break
	}
	if n != subscriberBuffer {
		t.Fatalf("fast-but-unread subscriber buffered %d events, want %d", n, subscriberBuffer)
	}
}

func TestCancelClosesAndUnsubscribes(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe(context.Background())
	if got := b.SubscriberCount(); got != 1 {
		t.Fatalf("SubscriberCount = %d, want 1", got)
	}

	cancel()
	cancel() // idempotent

	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount after cancel = %d, want 0", got)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel still open after cancel")
	}
	// Publishing after cancel must not panic (send-on-closed would).
	b.Publish(Event{Type: "repo.changed"})
}

func TestContextCancelUnsubscribes(t *testing.T) {
	b := NewBus()
	ctx, cancelCtx := context.WithCancel(context.Background())
	ch, cancel := b.Subscribe(ctx)
	defer cancel()

	cancelCtx()

	deadline := time.Now().Add(5 * time.Second)
	for b.SubscriberCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscriber not removed after context cancellation")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel still open after context cancellation")
	}
}

func TestWriteSSEFormat(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{
			name:  "envelope payload",
			event: Event{Type: "repo.changed", Payload: map[string]string{"type": "repo.changed"}},
			want:  "event: repo.changed\ndata: {\"type\":\"repo.changed\"}\n\n",
		},
		{
			name:  "nil payload",
			event: Event{Type: "heartbeat"},
			want:  "event: heartbeat\ndata: {}\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sb strings.Builder
			if err := WriteSSE(&sb, nil, tt.event); err != nil {
				t.Fatalf("WriteSSE: %v", err)
			}
			if sb.String() != tt.want {
				t.Fatalf("WriteSSE wrote %q, want %q", sb.String(), tt.want)
			}
		})
	}
}

func TestWriteSSEUnmarshalablePayload(t *testing.T) {
	var sb strings.Builder
	err := WriteSSE(&sb, nil, Event{Type: "x", Payload: func() {}})
	if err == nil {
		t.Fatal("want error for unmarshalable payload")
	}
	if sb.Len() != 0 {
		t.Fatalf("wrote %q despite marshal error", sb.String())
	}
}
