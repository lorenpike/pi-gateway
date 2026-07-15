package chat

import (
	"context"
	"errors"
	"strings"
	"time"

	"wall-e/rpc"
	"wall-e/turn"
)

var (
	errBufferedReplyIncomplete = errors.New("chat: response ended before completion")
	errBufferedReplyIdle       = errors.New("chat: response stream idle timeout")
)

type bufferedReply struct {
	Text       string
	Suppressed bool
}

// isNoReply recognizes the buffered-channel silent-completion marker. It must
// only be called with the authoritative complete response, never with a delta.
func isNoReply(text string) bool {
	return strings.TrimSpace(text) == "NO_REPLY"
}

// awaitBufferedReply consumes a subscription without exposing assistant
// deltas. The subscription remains owned by the caller and is not closed here.
func awaitBufferedReply(ctx context.Context, sub *turn.Subscription, idleTimeout time.Duration) (bufferedReply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sub == nil || sub.Events == nil || sub.FinalText == nil {
		return bufferedReply{}, errBufferedReplyIncomplete
	}

	var idle *time.Timer
	var idleC <-chan time.Time
	if idleTimeout > 0 {
		idle = time.NewTimer(idleTimeout)
		defer idle.Stop()
		idleC = idle.C
	}
	resetIdle := func() {
		if idle == nil {
			return
		}
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(idleTimeout)
	}

	events := sub.Events
	for events != nil {
		select {
		case <-ctx.Done():
			return bufferedReply{}, ctx.Err()
		case <-idleC:
			return bufferedReply{}, errBufferedReplyIdle
		case event, ok := <-events:
			if !ok {
				events = nil
				break
			}
			resetIdle()
			if event.Type == rpc.EventAgentEnd {
				outcome, err := rpc.DecodeAgentEndOutcome(event.Raw)
				if err != nil || !outcome.WillRetry {
					events = nil
				}
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return bufferedReply{}, ctx.Err()
		case <-idleC:
			return bufferedReply{}, errBufferedReplyIdle
		case final, ok := <-sub.FinalText:
			if !ok {
				return bufferedReply{}, errBufferedReplyIncomplete
			}
			return bufferedReply{Text: final, Suppressed: isNoReply(final)}, nil
		}
	}
}
