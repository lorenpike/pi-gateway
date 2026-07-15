package rpc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeAgentEndOutcome_ProviderError(t *testing.T) {
	raw := json.RawMessage(`{"type":"agent_end","messages":[{"role":"user"},{"role":"assistant","stopReason":"error","errorMessage":"402: insufficient credits"}],"willRetry":false}`)

	outcome, err := DecodeAgentEndOutcome(raw)
	if err != nil {
		t.Fatalf("DecodeAgentEndOutcome: %v", err)
	}
	if outcome.WillRetry {
		t.Fatal("WillRetry = true, want false")
	}
	if outcome.ErrorMessage != "402: insufficient credits" {
		t.Fatalf("ErrorMessage = %q", outcome.ErrorMessage)
	}
}

func TestDecodeAgentEndOutcome_Retry(t *testing.T) {
	raw := json.RawMessage(`{"type":"agent_end","messages":[{"role":"assistant","stopReason":"error","errorMessage":"overloaded"}],"willRetry":true}`)

	outcome, err := DecodeAgentEndOutcome(raw)
	if err != nil {
		t.Fatalf("DecodeAgentEndOutcome: %v", err)
	}
	if !outcome.WillRetry || outcome.ErrorMessage != "overloaded" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestDecodeAgentEndOutcome_FinalSuccess(t *testing.T) {
	raw := json.RawMessage(`{"type":"agent_end","messages":[{"role":"assistant","stopReason":"error","errorMessage":"first attempt failed"},{"role":"assistant","stopReason":"stop"}],"willRetry":false}`)

	outcome, err := DecodeAgentEndOutcome(raw)
	if err != nil {
		t.Fatalf("DecodeAgentEndOutcome: %v", err)
	}
	if outcome.WillRetry || outcome.ErrorMessage != "" {
		t.Fatalf("outcome = %+v, want success", outcome)
	}
}

func TestDecodeAgentEndOutcome_WrongEvent(t *testing.T) {
	_, err := DecodeAgentEndOutcome(json.RawMessage(`{"type":"message_end"}`))
	if err == nil || !strings.Contains(err.Error(), "unexpected event type") {
		t.Fatalf("error = %v", err)
	}
}
