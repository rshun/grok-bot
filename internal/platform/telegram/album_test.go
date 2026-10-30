package telegram

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rshun/grok-bot/internal/platform"
)

type handled struct {
	in    platform.Inbound
	reply platform.Replier
}

type sink struct {
	mu  sync.Mutex
	got []handled
	ch  chan struct{}
}

func newSink() *sink { return &sink{ch: make(chan struct{}, 64)} }

func (s *sink) handle(_ context.Context, in platform.Inbound, r platform.Replier) {
	s.mu.Lock()
	s.got = append(s.got, handled{in, r})
	s.mu.Unlock()
	s.ch <- struct{}{}
}

func (s *sink) wait(t *testing.T, n int) []handled {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-s.ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("handler called %d times, want %d", i, n)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]handled(nil), s.got...)
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

type namedReply string

func (namedReply) Send(context.Context, string, string) error { return nil }

func albumPhoto(id, caption string) platform.Inbound {
	return platform.Inbound{
		Platform: platform.Telegram, ChatID: "9", MessageID: id, UserID: "1", Text: caption, TextMessage: true,
		Images: []platform.Image{{MIME: "image/jpeg", Name: id}},
	}
}

func testAlbums(ctx context.Context, s *sink) *albums {
	a := newAlbums(ctx, s.handle)
	a.quiet = 30 * time.Millisecond
	return a
}

func cleanup(t *testing.T, a *albums) *albums {
	t.Cleanup(a.stop)
	return a
}

func TestTimerFlushedAlbumStaysAheadOfNextMessage(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	handler := func(_ context.Context, in platform.Inbound, _ platform.Replier) {
		if len(in.Images) > 0 {
			close(entered)
			<-release // the album is still being handed on when the text arrives
		}
		mu.Lock()
		order = append(order, in.MessageID)
		mu.Unlock()
	}
	a := cleanup(t, newAlbums(context.Background(), handler))
	a.quiet = 10 * time.Millisecond
	a.add(albumPhoto("1", "pics"), "g", namedReply("1"))
	<-entered
	done := make(chan struct{})
	go func() {
		a.add(platform.Inbound{ChatID: "9", MessageID: "2", Text: "and?", TextMessage: true}, "", namedReply("2"))
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "1" || order[1] != "2" {
		t.Fatalf("order %v", order)
	}
}

func TestWaitingAlbumsLeaveOldestFirst(t *testing.T) {
	for run := 0; run < 20; run++ { // map order is random; repeat to catch it
		s := newSink()
		a := cleanup(t, testAlbums(context.Background(), s))
		a.quiet = time.Hour
		for _, g := range []string{"g1", "g2", "g3"} {
			a.add(albumPhoto(g, ""), g, namedReply(g))
		}
		a.add(platform.Inbound{ChatID: "9", MessageID: "t", Text: "hi", TextMessage: true}, "", namedReply("t"))
		got := s.wait(t, 4)
		for i, want := range []string{"g1", "g2", "g3", "t"} {
			if got[i].in.MessageID != want {
				t.Fatalf("run %d: position %d is %s, want %s", run, i, got[i].in.MessageID, want)
			}
		}
	}
}

func TestAlbumMergesGroup(t *testing.T) {
	s := newSink()
	a := cleanup(t, testAlbums(context.Background(), s))
	a.add(albumPhoto("1", ""), "g", namedReply("1"))
	a.add(albumPhoto("2", "what are these"), "g", namedReply("2"))
	a.add(albumPhoto("3", ""), "g", namedReply("3"))
	got := s.wait(t, 1)
	in := got[0].in
	if len(in.Images) != 3 || in.Text != "what are these" || in.MessageID != "2" || !in.TextMessage || in.Skipped {
		t.Fatalf("%#v", in)
	}
	if got[0].reply != namedReply("2") {
		t.Fatalf("reply goes to %v", got[0].reply)
	}
	if in.Images[0].Name != "1" || in.Images[2].Name != "3" {
		t.Fatalf("order lost: %#v", in.Images)
	}
	time.Sleep(60 * time.Millisecond)
	if s.count() != 1 {
		t.Fatalf("handler called %d times", s.count())
	}
}

func TestAlbumWithoutCaptionUsesFirst(t *testing.T) {
	s := newSink()
	a := cleanup(t, testAlbums(context.Background(), s))
	a.add(albumPhoto("4", ""), "g", namedReply("4"))
	a.add(albumPhoto("5", ""), "g", namedReply("5"))
	got := s.wait(t, 1)
	if got[0].in.MessageID != "4" || got[0].reply != namedReply("4") || len(got[0].in.Images) != 2 {
		t.Fatalf("%#v", got[0])
	}
}

func TestAlbumKeepsAtMostMaxImages(t *testing.T) {
	s := newSink()
	a := cleanup(t, testAlbums(context.Background(), s))
	for i := 0; i < platform.MaxImages+2; i++ {
		a.add(albumPhoto(string(rune('a'+i)), ""), "g", namedReply("x"))
	}
	got := s.wait(t, 1)
	if len(got[0].in.Images) != platform.MaxImages || !got[0].in.Skipped {
		t.Fatalf("images=%d skipped=%v", len(got[0].in.Images), got[0].in.Skipped)
	}
}

func TestAlbumWithoutPicturesIsNotUsable(t *testing.T) {
	s := newSink()
	a := cleanup(t, testAlbums(context.Background(), s))
	video := platform.Inbound{ChatID: "9", MessageID: "1", Text: "look", Skipped: true}
	a.add(video, "g", namedReply("1"))
	a.add(platform.Inbound{ChatID: "9", MessageID: "2", Skipped: true}, "g", namedReply("2"))
	got := s.wait(t, 1)
	if got[0].in.TextMessage || len(got[0].in.Images) != 0 || !got[0].in.Skipped {
		t.Fatalf("%#v", got[0].in)
	}
}

func TestNonAlbumPassesThrough(t *testing.T) {
	s := newSink()
	a := cleanup(t, testAlbums(context.Background(), s))
	a.quiet = time.Hour
	a.add(platform.Inbound{ChatID: "9", MessageID: "1", Text: "hi", TextMessage: true}, "", namedReply("1"))
	if s.count() != 1 {
		t.Fatalf("plain message not handed on at once: %d", s.count())
	}
}

func TestPlainMessageFlushesWaitingAlbumFirst(t *testing.T) {
	s := newSink()
	a := cleanup(t, testAlbums(context.Background(), s))
	a.quiet = time.Hour
	a.add(albumPhoto("1", "pics"), "g", namedReply("1"))
	a.add(albumPhoto("2", ""), "g", namedReply("2"))
	a.add(platform.Inbound{ChatID: "8", MessageID: "7", Text: "other chat", TextMessage: true}, "", namedReply("7"))
	if s.count() != 1 {
		t.Fatalf("another chat's message must not flush the album: %d", s.count())
	}
	a.add(platform.Inbound{ChatID: "9", MessageID: "3", Text: "and?", TextMessage: true}, "", namedReply("3"))
	got := s.wait(t, 3)
	if got[1].in.MessageID != "1" || len(got[1].in.Images) != 2 || got[2].in.MessageID != "3" {
		t.Fatalf("order: %#v", got)
	}
	a.stop()
}

func TestAlbumFlushOnStop(t *testing.T) {
	s := newSink()
	ctx, cancel := context.WithCancel(context.Background())
	a := testAlbums(ctx, s)
	a.add(albumPhoto("1", ""), "g", namedReply("1"))
	cancel()
	a.stop()
	a.add(albumPhoto("2", ""), "g", namedReply("2"))
	time.Sleep(80 * time.Millisecond)
	if s.count() != 0 {
		t.Fatalf("dropped album was handed on: %d", s.count())
	}
}
