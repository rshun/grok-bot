package telegram

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/rshun/grok-bot/internal/platform"
)

// albumQuiet is how long an album waits for more of its messages before it is handed on.
const albumQuiet = 1500 * time.Millisecond

// albums merges the messages of one Telegram album (same media_group_id) into a single
// Inbound. Telegram sends every picture of an album as its own message, close together, and
// the caption sits on only one of them.
type albums struct {
	ctx     context.Context
	handler func(context.Context, platform.Inbound, platform.Replier)
	quiet   time.Duration

	// deliverMu serializes every handler call made through albums, so a timer-flushed album
	// and the plain message after it reach the handler in order. Lock order: deliverMu, mu.
	deliverMu sync.Mutex

	mu      sync.Mutex
	groups  map[string]*album // key: chat ID + "/" + group ID
	seq     uint64
	stopped bool
}

type album struct {
	chatID string
	seq    uint64 // creation order, so waiting albums of one chat leave in order
	parts  []albumPart
	timer  *time.Timer
}

type albumPart struct {
	in    platform.Inbound
	reply platform.Replier
}

func newAlbums(ctx context.Context, handler func(context.Context, platform.Inbound, platform.Replier)) *albums {
	return &albums{ctx: ctx, handler: handler, quiet: albumQuiet, groups: map[string]*album{}}
}

// add takes one message. Messages without a group go straight to the handler, after any album
// still waiting in the same chat, so the chat keeps its order. in.Text of an album message is
// its caption.
func (a *albums) add(in platform.Inbound, groupID string, reply platform.Replier) {
	if groupID == "" {
		a.deliverMu.Lock()
		defer a.deliverMu.Unlock()
		for _, g := range a.takeChat(in.ChatID) {
			a.deliver(g)
		}
		if !a.isStopped() {
			a.handler(a.ctx, in, reply)
		}
		return
	}
	key := in.ChatID + "/" + groupID
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return
	}
	g := a.groups[key]
	if g == nil {
		a.seq++
		g = &album{chatID: in.ChatID, seq: a.seq}
		a.groups[key] = g
		g.timer = time.AfterFunc(a.quiet, func() { a.flush(key, g) })
	} else {
		g.timer.Reset(a.quiet)
	}
	g.parts = append(g.parts, albumPart{in: in, reply: reply})
}

// flush hands on one album once it has been quiet long enough.
func (a *albums) flush(key string, g *album) {
	a.deliverMu.Lock()
	defer a.deliverMu.Unlock()
	a.mu.Lock()
	if a.stopped || a.groups[key] != g {
		a.mu.Unlock()
		return
	}
	delete(a.groups, key)
	a.mu.Unlock()
	a.deliver(g)
}

func (a *albums) isStopped() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stopped
}

// takeChat removes and returns the albums still waiting in one chat, oldest first.
func (a *albums) takeChat(chatID string) []*album {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*album
	for key, g := range a.groups {
		if g.chatID == chatID {
			g.timer.Stop()
			delete(a.groups, key)
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// stop drops every waiting album. It waits for a delivery already under way, so nothing is
// handed on after it returns.
func (a *albums) stop() {
	a.deliverMu.Lock()
	defer a.deliverMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopped = true
	for key, g := range a.groups {
		g.timer.Stop()
		delete(a.groups, key)
	}
}

func (a *albums) deliver(g *album) {
	if a.ctx.Err() != nil || len(g.parts) == 0 {
		return
	}
	in, reply := mergeAlbum(g.parts)
	a.handler(a.ctx, in, reply)
}

// mergeAlbum joins the parts in arrival order. The message with the caption is the one replied
// to; without a caption it is the first.
func mergeAlbum(parts []albumPart) (platform.Inbound, platform.Replier) {
	lead := parts[0]
	for _, p := range parts {
		if p.in.Text != "" {
			lead = p
			break
		}
	}
	in := lead.in
	in.Images = nil
	in.Skipped = false
	for _, p := range parts {
		in.Images = append(in.Images, p.in.Images...)
		in.Skipped = in.Skipped || p.in.Skipped
	}
	if len(in.Images) > platform.MaxImages {
		in.Images = in.Images[:platform.MaxImages]
		in.Skipped = true
	}
	// An album with no picture Grok can take (videos only, say) is not a usable message.
	in.TextMessage = len(in.Images) > 0
	return in, lead.reply
}
