package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"wall-e/pool"
	"wall-e/rpc"
	"wall-e/turn"
)

// gatewayCommand describes a command implemented by wall-e rather than pi.
type gatewayCommand struct {
	Name        string
	Description string
}

var gatewayNativeCommands = []gatewayCommand{
	{Name: "skill", Description: "List skills or run /skill <name> [args]"},
	{Name: "name", Description: "Set or clear this pi session name"},
	{Name: "session", Description: "Show current pi session info"},
	{Name: "clone", Description: "Clone this pi session branch"},
	{Name: "new", Description: "Start a new pi session"},
	{Name: "compact", Description: "Compact this pi session context"},
	{Name: "abort", Description: "Abort the current pi response"},
}

// gatewayCommandError retains the RPC response so adapters can render a
// platform-appropriate, useful error without duplicating command execution.
type gatewayCommandError struct {
	Name     string
	Response rpc.Response
	Err      error
}

func (e *gatewayCommandError) Error() string {
	msg := "/" + e.Name + " failed"
	if e.Err != nil {
		return msg + ": " + e.Err.Error()
	}
	if e.Response.Error != "" {
		return msg + ": " + e.Response.Error
	}
	return msg
}

func (e *gatewayCommandError) Unwrap() error { return e.Err }

// executeGatewayCommand implements commands shared by Telegram and Discord.
// Busy checks and /skill rewriting remain adapter concerns because their
// acknowledgement and argument models differ.
func executeGatewayCommand(ctx context.Context, p *pool.Pool, turns *turn.Manager, chID pool.ChannelID, name, args string) (string, error) {
	args = strings.TrimSpace(args)
	if name == "abort" {
		resp, err := turns.Abort(ctx, chID)
		if errors.Is(err, turn.ErrNoActiveTurn) {
			return "No active pi turn to abort.", nil
		}
		if err != nil || !resp.Success {
			return "", &gatewayCommandError{Name: name, Response: resp, Err: err}
		}
		return "Aborted current pi turn.", nil
	}

	slot, err := p.Acquire(ctx, chID)
	if err != nil {
		return "", fmt.Errorf("no agent available: %w", err)
	}
	defer p.Release(chID)

	client := slot.Client()
	switch name {
	case "name":
		resp, err := client.SetSessionName(ctx, args)
		if err != nil || !resp.Success {
			return "", &gatewayCommandError{Name: name, Response: resp, Err: err}
		}
		if args == "" {
			return "Session name cleared.", nil
		}
		return "Session name set to: " + args, nil
	case "session":
		st, err := client.GetState(ctx)
		if err != nil {
			return "", &gatewayCommandError{Name: name, Err: err}
		}
		return fmt.Sprintf("Session\nID: %s\nName: %s\nMessages: %d\nStreaming: %v", emptyDash(st.SessionID), emptyDash(st.SessionName), st.MessageCount, st.IsStreaming), nil
	case "new":
		newPath := p.NewSessionPath(chID)
		resp, _, err := client.SwitchSession(ctx, newPath)
		if err != nil || !resp.Success {
			return "", &gatewayCommandError{Name: name, Response: resp, Err: err}
		}
		if err := p.ResyncFromState(chID, newPath); err != nil {
			return "", &gatewayCommandError{Name: name, Err: err}
		}
		return "Started a new pi session.", nil
	case "clone":
		resp, st, err := client.Clone(ctx)
		if err != nil || !resp.Success {
			return "", &gatewayCommandError{Name: name, Response: resp, Err: err}
		}
		clonePath := p.NewSessionPath(chID)
		if st.SessionFile == "" {
			return "", &gatewayCommandError{Name: name, Err: errors.New("clone returned empty session file")}
		}
		if err := p.CopySessionFile(st.SessionFile, clonePath); err != nil {
			return "", &gatewayCommandError{Name: name, Err: err}
		}
		resp, _, err = client.SwitchSession(ctx, clonePath)
		if err != nil || !resp.Success {
			return "", &gatewayCommandError{Name: name, Response: resp, Err: err}
		}
		if err := p.ResyncFromState(chID, clonePath); err != nil {
			return "", &gatewayCommandError{Name: name, Err: err}
		}
		if st.SessionFile != clonePath {
			_ = p.RemoveSessionFile(st.SessionFile)
		}
		return "Cloned this pi session branch.", nil
	case "compact":
		resp, err := client.Compact(ctx, args)
		if err != nil || !resp.Success {
			return "", &gatewayCommandError{Name: name, Response: resp, Err: err}
		}
		return "Compacted this pi session.", nil
	default:
		return "", fmt.Errorf("unknown gateway command /%s", name)
	}
}
