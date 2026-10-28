package core

import (
	"context"
	"errors"
	"log/slog"

	"github.com/rshun/grok-bot/internal/actions"
	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
)

// noReply is what Grok answers with after reacting when there is nothing to add to the reaction.
const noReply = "[[NO_REPLY]]"

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
	// Scheduled runs have no message to react to.
	if reactor, ok := out.(platform.Reactor); ok && in.MessageID != "" {
		turn.React = func(ctx context.Context, emoji string) error {
			err := reactor.React(ctx, in, emoji)
			var toolErr *actions.ToolError
			if err != nil && !errors.As(err, &toolErr) {
				// Error values can embed request URLs, so only the platform is logged.
				slog.Warn("reaction failed", "platform", in.Platform)
			}
			return err
		}
	}
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
