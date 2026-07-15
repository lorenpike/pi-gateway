package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"wall-e/rpc"
	"wall-e/turn"
)

func TestIsNoReply(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"NO_REPLY", true},
		{"  NO_REPLY\n", true},
		{"\u2003NO_REPLY\u00a0", true},
		{"no_reply", false},
		{"`NO_REPLY`", false},
		{"I will use NO_REPLY when appropriate.", false},
		{"NO_REPLY.", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := isNoReply(tt.text); got != tt.want {
				t.Fatalf("isNoReply(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func testSubscription(events <-chan rpc.Event, final <-chan string) *turn.Subscription {
	return &turn.Subscription{Events: events, FinalText: final}
}

func terminalAgentEnd() rpc.Event {
	return rpc.Event{Type: rpc.EventAgentEnd, Raw: []byte(`{"type":"agent_end"}`)}
}

func TestAwaitBufferedReplyUsesAuthoritativeFinalText(t *testing.T) {
	events := make(chan rpc.Event, 4)
	final := make(chan string, 1)
	events <- rpc.Event{Type: rpc.EventMessageUpdate}
	events <- rpc.Event{Type: rpc.EventMessageUpdate}
	events <- terminalAgentEnd()
	final <- "authoritative response"
	close(final)

	reply, err := awaitBufferedReply(context.Background(), testSubscription(events, final), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "authoritative response" || reply.Suppressed {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestAwaitBufferedReplySuppression(t *testing.T) {
	for _, text := range []string{"NO_REPLY", "\n\u2003NO_REPLY\t"} {
		t.Run(text, func(t *testing.T) {
			events := make(chan rpc.Event, 1)
			final := make(chan string, 1)
			events <- terminalAgentEnd()
			final <- text
			reply, err := awaitBufferedReply(context.Background(), testSubscription(events, final), time.Second)
			if err != nil || !reply.Suppressed || reply.Text != text {
				t.Fatalf("reply=%+v err=%v", reply, err)
			}
		})
	}
}

func TestAwaitBufferedReplyEventClosure(t *testing.T) {
	t.Run("with final", func(t *testing.T) {
		events := make(chan rpc.Event)
		final := make(chan string, 1)
		close(events)
		final <- "complete"
		close(final)
		reply, err := awaitBufferedReply(context.Background(), testSubscription(events, final), time.Second)
		if err != nil || reply.Text != "complete" {
			t.Fatalf("reply=%+v err=%v", reply, err)
		}
	})
	t.Run("without final", func(t *testing.T) {
		events := make(chan rpc.Event)
		final := make(chan string)
		close(events)
		close(final)
		_, err := awaitBufferedReply(context.Background(), testSubscription(events, final), time.Second)
		if !errors.Is(err, errBufferedReplyIncomplete) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestAwaitBufferedReplyCancellationAndIdle(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		events := make(chan rpc.Event)
		final := make(chan string)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := awaitBufferedReply(ctx, testSubscription(events, final), time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("idle", func(t *testing.T) {
		events := make(chan rpc.Event)
		final := make(chan string)
		_, err := awaitBufferedReply(context.Background(), testSubscription(events, final), 15*time.Millisecond)
		if !errors.Is(err, errBufferedReplyIdle) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestAwaitBufferedReplyAnyEventResetsIdleWatchdog(t *testing.T) {
	events := make(chan rpc.Event)
	final := make(chan string, 1)
	go func() {
		for range 3 {
			time.Sleep(10 * time.Millisecond)
			events <- rpc.Event{Type: rpc.EventToolExecutionUpdate, Raw: []byte(`{"type":"tool_execution_update"}`)}
		}
		events <- terminalAgentEnd()
		final <- "done"
	}()
	reply, err := awaitBufferedReply(context.Background(), testSubscription(events, final), 20*time.Millisecond)
	if err != nil || reply.Text != "done" {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
}
