package core

import (
	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
)

// beginTurn opens the MCP channel for one grok run. It returns the turn, the
// environment entries that hand its token to grok, and a function that ends the
// turn. With no MCP server it returns a nil turn, no env and a no-op end.
// The callbacks that make tools available (react, send_file) are set on the
// turn here as they are added; a tool whose callback is nil is not offered.
func (s *Service) beginTurn(in platform.Inbound, out platform.Replier, fileTools bool) (*actions.Turn, []string, func()) {
	if s.Actions == nil {
		return nil, nil, func() {}
	}
	turn := &actions.Turn{}
	env, end := s.Actions.Begin(turn)
	return turn, []string{env}, end
}

// withTurn adds the channel fields for turn to opts.
func withTurn(opts grok.ReplyOptions, turn *actions.Turn, env []string) grok.ReplyOptions {
	if turn == nil {
		return opts
	}
	opts.Actions = true
	opts.React = turn.React != nil
	opts.Env = env
	return opts
}
