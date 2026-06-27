// Package rpc contains the wall-e ↔ pi JSONL RPC protocol layer.
//
// extui.go implements the auto-answer policy for the extension-UI sub-protocol
// described in docs/rpc.md ("Extension UI Protocol"). The gateway is
// non-interactive, so each dialog/request method is answered with a fixed,
// policy-derived response instead of surfacing a prompt to a human.
package rpc

import "encoding/json"

// ExtensionUIPolicy controls how the gateway auto-answers extension UI dialogs.
//
// The policy matches §3 ("Decisions summary") of the gateway plan:
//   - confirm → respond with ConfirmedDefault (default true, configurable)
//   - select  → respond with the first option
//   - input   → respond cancelled
//   - editor  → respond cancelled
//   - notify / setStatus / setWidget / setTitle / set_editor_text → ignored
//
// Dialog responses are sent for dialog methods only; fire-and-forget methods
// produce no response (they would block the agent otherwise).
type ExtensionUIPolicy struct {
	// ConfirmedDefault is the auto-answer for `confirm` dialogs.
	ConfirmedDefault bool
}

// DefaultExtensionUIPolicy returns the v1 policy from the plan: confirm=true.
func DefaultExtensionUIPolicy() ExtensionUIPolicy {
	return ExtensionUIPolicy{ConfirmedDefault: true}
}

// Dialog methods — these expect a response.
const (
	UISelect = "select"
	UIConfirm = "confirm"
	UIInput = "input"
	UIEditor = "editor"
)

// Fire-and-forget methods — these expect no response.
const (
	UINotify       = "notify"
	UISetStatus    = "setStatus"
	UISetWidget    = "setWidget"
	UISetTitle     = "setTitle"
	UISetEditorText = "set_editor_text"
)

// IsDialog reports whether method requires an extension_ui_response.
func IsDialog(method string) bool {
	switch method {
	case UISelect, UIConfirm, UIInput, UIEditor:
		return true
	}
	return false
}

// uiRequest is the inbound `extension_ui_request` envelope.
type uiRequest struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Title  string          `json:"title,omitempty"`
	// select
	Options []string `json:"options,omitempty"`
	// input / editor
	Placeholder string `json:"placeholder,omitempty"`
	Prefill     string `json:"prefill,omitempty"`
	// notify
	NotifyType string `json:"notifyType,omitempty"`
	// fire-and-forget extras captured for logging only
	Raw json.RawMessage `json:"-"`
}

// BuildUIResponse returns the JSONL bytes of the extension_ui_response envelope
// for the given request, or nil if the request is fire-and-forget.
func (p ExtensionUIPolicy) BuildUIResponse(req []byte) ([]byte, error) {
	var r uiRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return nil, err
	}
	if !IsDialog(r.Method) {
		return nil, nil
	}

	resp := struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Value     string `json:"value,omitempty"`
		Confirmed *bool  `json:"confirmed,omitempty"`
		Cancelled bool   `json:"cancelled,omitempty"`
	}{
		Type: "extension_ui_response",
		ID:   r.ID,
	}

	switch r.Method {
	case UIConfirm:
		b := p.ConfirmedDefault
		resp.Confirmed = &b
	case UISelect:
		if len(r.Options) > 0 {
			resp.Value = r.Options[0]
		} else {
			// No options to pick — cancel rather than send an empty value.
			resp.Cancelled = true
		}
	case UIInput, UIEditor:
		// Non-interactive gateway: never provide free-form text.
		resp.Cancelled = true
	}

	return json.Marshal(resp)
}
