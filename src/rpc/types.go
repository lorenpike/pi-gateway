// Package rpc contains the wall-e ↔ pi JSONL RPC protocol layer.
//
// This file defines the typed structures for commands, responses, events and
// the extension-UI sub-protocol described in docs/rpc.md. The framing primitive
// lives in framing.go; the process-owning client lives in client.go.
package rpc

import "encoding/json"

// ErrPiExit is returned by Client methods (and Wait) when the underlying
// `pi --mode rpc` process has exited, i.e. its stdout closed before a response
// could be delivered. It is a sentinel so callers can distinguish "the process
// died" from "the command failed" via errors.Is.
//
// We deliberately use a distinct type rather than reusing a stdlib sentinel so
// callers can do `errors.Is(err, rpc.ErrPiExit)`.
var errPiExit = piExitError{}

// ErrPiExit is the sentinel returned when the pi process exits.
var ErrPiExit = errPiExit

type piExitError struct{}

func (piExitError) Error() string { return "rpc: pi process exited" }
func (piExitError) Is(target error) bool {
	_, ok := target.(piExitError)
	return ok
}

// ImageContent mirrors the `ImageContent` block used by `prompt`/`steer`/
// `follow_up`. See docs/rpc.md "prompt".
type ImageContent struct {
	Type     string `json:"type"`     // always "image"
	Data     string `json:"data"`     // base64-encoded image bytes
	MimeType string `json:"mimeType"` // e.g. "image/png"
}

// Response is the decoded form of a `{"type":"response",...}` reply from pi.
//
// All commands receive exactly one Response (correlated by ID when the caller
// supplied one). Failures *after* acceptance are reported through the event
// stream, not as a second Response for the same ID (per docs/rpc.md).
type Response struct {
	ID      string          `json:"id,omitempty"`
	Command string          `json:"command"`
	Success bool           `json:"success"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// State is the `data` payload of a `get_state` response.
type State struct {
	Model                 json.RawMessage `json:"model"`
	ThinkingLevel         string          `json:"thinkingLevel"`
	IsStreaming           bool            `json:"isStreaming"`
	IsCompacting          bool            `json:"isCompacting"`
	SteeringMode          string          `json:"steeringMode"`
	FollowUpMode          string          `json:"followUpMode"`
	SessionFile           string          `json:"sessionFile"`
	SessionID             string          `json:"sessionId"`
	SessionName            string          `json:"sessionName,omitempty"`
	AutoCompactionEnabled bool            `json:"autoCompactionEnabled"`
	MessageCount          int             `json:"messageCount"`
	PendingMessageCount   int             `json:"pendingMessageCount"`
}

// Event is a single non-response message streamed by pi (agent_start,
// message_update, tool_execution_*, extension_ui_request, etc.).
//
// The raw JSONL frame is preserved in Raw so higher layers can decode into
// concrete event structs as needed without forcing this package to model every
// event variant up front.
type Event struct {
	// Type is the value of the JSON `type` field (e.g. "agent_end",
	// "message_update"). For responses it would be "response" but responses are
	// routed to their callers and never appear on Events.
	Type string

	// Raw is the complete JSONL frame bytes (without the line terminator).
	Raw json.RawMessage
}

// EventType constants for the events wall-e currently cares about. The list is
// not exhaustive; unknown types are still delivered on Events with their Type
// set to whatever pi sent.
const (
	EventAgentStart          = "agent_start"
	EventAgentEnd            = "agent_end"
	EventTurnStart           = "turn_start"
	EventTurnEnd             = "turn_end"
	EventMessageStart        = "message_start"
	EventMessageUpdate       = "message_update"
	EventMessageEnd          = "message_end"
	EventToolExecutionStart  = "tool_execution_start"
	EventToolExecutionUpdate  = "tool_execution_update"
	EventToolExecutionEnd    = "tool_execution_end"
	EventQueueUpdate         = "queue_update"
	EventCompactionStart     = "compaction_start"
	EventCompactionEnd       = "compaction_end"
	EventAutoRetryStart      = "auto_retry_start"
	EventAutoRetryEnd        = "auto_retry_end"
	EventExtensionError      = "extension_error"
)

// AssistantMessageEvent is the `assistantMessageEvent` field of a
// `message_update` event. Only the fields wall-e currently consumes are typed;
// the rest live in Raw.
type AssistantMessageEvent struct {
	Type         string          `json:"type"`
	ContentIndex int             `json:"contentIndex,omitempty"`
	Delta        string          `json:"delta,omitempty"`
	Content      string          `json:"content,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	Raw          json.RawMessage `json:"-"`
}
