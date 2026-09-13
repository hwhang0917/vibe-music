package player

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hwhang0917/vibe-music/internal/source"
	"github.com/hwhang0917/vibe-music/internal/source/fake"
)

var (
	a  = source.Track{ID: "a", Title: "A", Source: "fake"}
	b  = source.Track{ID: "b", Title: "B", Source: "fake"}
	c  = source.Track{ID: "c", Title: "C", Source: "fake"}
	g1 = Guest{ID: "g1", Name: "Kim"}
	g2 = Guest{ID: "g2", Name: "Lee"}
	g3 = Guest{ID: "g3", Name: "Park"}
)

func setup(t *testing.T) (*Player, *fake.Source) {
	t.Helper()
	f := fake.New(a, b, c)
	p := New(Options{Sources: []source.Source{f}})
	if err := p.SetEnabled(context.Background(), "fake", true); err != nil {
		t.Fatal(err)
	}
	return p, f
}

func queueIDs(s State) []string {
	var ids []string
	for _, it := range s.Queue {
		ids = append(ids, it.Track.ID)
	}
	return ids
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestFirstRequestPlaysImmediately(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	_ = p.Request(ctx, a, g1)
	eq(t, f.Played(), []string{"a"})
	s := p.State()
	if s.NowPlaying == nil || s.NowPlaying.Track.ID != "a" || s.NowPlaying.RequestedBy != "Kim" {
		t.Fatalf("now playing: %+v", s.NowPlaying)
	}
	if len(s.Queue) != 0 {
		t.Fatalf("queue should be empty, got %v", queueIDs(s))
	}
}

func TestVotesOrderQueueThenRequestTime(t *testing.T) {
	p, _ := setup(t)
	ctx := context.Background()
	_ = p.Request(ctx, a, g1) // playing
	_ = p.Request(ctx, b, g1)
	time.Sleep(time.Millisecond)
	_ = p.Request(ctx, c, g2)
	eq(t, queueIDs(p.State()), []string{"b", "c"})

	// the same song can be requested again: separate entries, oldest first
	_ = p.Request(ctx, c, g3)
	s := p.State()
	eq(t, queueIDs(s), []string{"b", "c", "c"})
	if s.Queue[1].ID == s.Queue[2].ID || s.Queue[1].RequestedByName != "Lee" || s.Queue[2].RequestedByName != "Park" {
		t.Fatalf("duplicates should be distinct items: %+v", s.Queue)
	}

	// votes reorder; voting twice as the same guest counts once
	cID := s.Queue[2].ID
	_ = p.Vote(cID, g1.ID)
	_ = p.Vote(cID, g2.ID)
	_ = p.Vote(cID, g2.ID)
	s = p.State()
	eq(t, queueIDs(s), []string{"c", "b", "c"})
	if s.Queue[0].Votes != 3 || s.Queue[0].ID != cID {
		t.Fatalf("votes = %d id=%s", s.Queue[0].Votes, s.Queue[0].ID)
	}
	if err := p.Vote("nope", g1.ID); err == nil {
		t.Fatal("vote on unknown item should fail")
	}
}

func TestAutoAdvanceOnEnded(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	_ = p.Request(ctx, a, g1)
	_ = p.Request(ctx, b, g1)
	p.Tick(ctx)
	eq(t, f.Played(), []string{"a"}) // still playing a
	f.FinishTrack()
	p.Tick(ctx)
	eq(t, f.Played(), []string{"a", "b"})
	f.FinishTrack()
	p.Tick(ctx)
	eq(t, f.Played(), []string{"a", "b"}) // queue empty -> stop
	if p.State().NowPlaying != nil {
		t.Fatal("expected nothing playing")
	}
	if f.Calls[len(f.Calls)-1] != "stop" {
		t.Fatalf("expected stop, calls=%v", f.Calls)
	}
}

func TestSkipThreshold(t *testing.T) {
	SkipGrace = 20 * time.Millisecond
	defer func() { SkipGrace = defaultSkipGrace }()
	p, f := setup(t)
	ctx := context.Background()
	_ = p.Request(ctx, a, g1)
	_ = p.Request(ctx, b, g1)

	p.SetConnectedGuests(4) // ceil(0.5*4) = 2
	if p.State().SkipThreshold != 2 {
		t.Fatalf("threshold = %d", p.State().SkipThreshold)
	}
	if p.VoteSkip(ctx, g1.ID) {
		t.Fatal("one vote should not skip")
	}
	if p.VoteSkip(ctx, g1.ID) {
		t.Fatal("same guest twice should not skip")
	}
	if !p.VoteSkip(ctx, g2.ID) {
		t.Fatal("second guest should skip")
	}
	time.Sleep(3 * SkipGrace) // the skip lands after the grace period
	eq(t, f.Played(), []string{"a", "b"})
	if p.State().SkipVotes != 0 {
		t.Fatal("skip votes should reset on advance")
	}

	p.SetConnectedGuests(0) // min threshold 1
	if p.State().SkipThreshold != 1 {
		t.Fatalf("threshold = %d", p.State().SkipThreshold)
	}
	if !p.VoteSkip(ctx, g3.ID) {
		t.Fatal("single vote should skip with no guests connected")
	}
	time.Sleep(3 * SkipGrace)
	if p.VoteSkip(ctx, g3.ID) {
		t.Fatal("nothing playing: no skip")
	}
}

func TestMixedSourcesSwitchPerTrack(t *testing.T) {
	f1 := fake.New(a, b)
	f2 := fake.New(c).WithID("two")
	p := New(Options{Sources: []source.Source{f1, f2}})
	ctx := context.Background()
	if err := p.SetEnabled(ctx, "fake", true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetEnabled(ctx, "two", true); err != nil {
		t.Fatal(err)
	}
	c2 := f2.Library[0]
	_ = p.Request(ctx, a, g1)  // plays on f1
	_ = p.Request(ctx, c2, g2) // queued, lives on f2
	f1.FinishTrack()
	p.Tick(ctx)
	eq(t, f2.Played(), []string{"c"})
	if f1.Calls[len(f1.Calls)-1] != "stop" {
		t.Fatalf("f1 should be stopped before f2 plays: %v", f1.Calls)
	}
	if got := p.State().NowPlaying.Track.Source; got != "two" {
		t.Fatalf("now playing source = %q", got)
	}

	// disabling the playing source stops it, drops only its items, announces
	_ = p.Request(ctx, b, g1)
	_ = p.Request(ctx, c2, g1)
	ch, cancel := p.Subscribe()
	defer cancel()
	<-ch
	if err := p.SetEnabled(ctx, "two", false); err != nil {
		t.Fatal(err)
	}
	s := <-ch
	if s.NowPlaying != nil || len(s.Queue) != 1 || s.Queue[0].Track.ID != "b" {
		t.Fatalf("after disable: %+v", s)
	}
	if s.Event == nil || s.Event.Type != "source_disabled" {
		t.Fatalf("event: %+v", s.Event)
	}
	if err := p.Request(ctx, c2, g1); err != ErrNoSource {
		t.Fatalf("request for a disabled source: %v", err)
	}
	// the remaining head starts on the next tick
	p.Tick(ctx)
	eq(t, f1.Played(), []string{"a", "b"})
	if err := p.SetEnabled(ctx, "nope", true); err == nil {
		t.Fatal("unknown source should error")
	}
}

func TestExclusiveSource(t *testing.T) {
	f1 := fake.New(a)
	sp := fake.New(c).WithID("spotify")
	p := New(Options{Sources: []source.Source{f1, sp}})
	ctx := context.Background()
	_ = p.SetEnabled(ctx, "fake", true)
	if err := p.SetEnabled(ctx, "spotify", true); err != ErrExclusive {
		t.Fatalf("spotify next to another source: %v", err)
	}
	_ = p.SetEnabled(ctx, "fake", false)
	if err := p.SetEnabled(ctx, "spotify", true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetEnabled(ctx, "fake", true); err != ErrExclusive {
		t.Fatalf("another source next to spotify: %v", err)
	}
	if _, err := p.Search(ctx, "", "x", 5); err != nil {
		t.Fatalf("single enabled source is the default for search: %v", err)
	}
}

func TestBroadcastOnlyOnChange(t *testing.T) {
	p, _ := setup(t)
	ctx := context.Background()
	ch, cancel := p.Subscribe()
	defer cancel()
	<-ch // initial snapshot
	p.Tick(ctx)
	select {
	case s := <-ch:
		t.Fatalf("unexpected broadcast: %+v", s)
	default:
	}
	_ = p.Request(ctx, a, g1)
	select {
	case s := <-ch:
		if s.NowPlaying == nil || s.NowPlaying.Track.ID != "a" {
			t.Fatalf("bad state: %+v", s)
		}
	default:
		t.Fatal("expected broadcast after request")
	}
}

// named wraps a fake with a different ID so two can coexist.
type named struct {
	*fake.Source
	id string
}

func (n *named) ID() string { return n.id }

func TestRemoveGuestDropsRequestsAndVotes(t *testing.T) {
	p, _ := setup(t)
	ctx := context.Background()
	_ = p.Request(ctx, a, g1) // playing
	_ = p.Request(ctx, b, g2)
	_ = p.Request(ctx, c, g1)
	_ = p.Vote(p.State().Queue[0].ID, g1.ID) // g1 upvotes b
	p.SetConnectedGuests(4)
	p.VoteSkip(ctx, g1.ID)

	p.RemoveGuest(g1.ID)
	s := p.State()
	eq(t, queueIDs(s), []string{"b"})
	if s.Queue[0].Votes != 1 || s.SkipVotes != 0 {
		t.Fatalf("votes=%d skip=%d", s.Queue[0].Votes, s.SkipVotes)
	}
}

func TestRemoveOwnOrAdmin(t *testing.T) {
	p, _ := setup(t)
	ctx := context.Background()
	_ = p.Request(ctx, a, g1) // playing
	_ = p.Request(ctx, b, g1)
	_ = p.Request(ctx, c, g2)
	bID, cID := p.State().Queue[0].ID, p.State().Queue[1].ID

	if err := p.Remove(bID, g2.ID, false); err != ErrNotOwner {
		t.Fatalf("g2 removing g1's request: %v", err)
	}
	if err := p.Remove(bID, g1.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := p.Remove(cID, g1.ID, true); err != nil {
		t.Fatal("admin should remove anything:", err)
	}
	if err := p.Remove(cID, g1.ID, true); err != ErrNotInQueue {
		t.Fatalf("gone already: %v", err)
	}
	if len(p.State().Queue) != 0 {
		t.Fatal("queue should be empty")
	}
}

func TestDurationLearnedAtPlayIsBroadcast(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	ch, cancel := p.Subscribe()
	defer cancel()
	<-ch
	_ = p.Request(ctx, a, g1) // duration unknown (0) at request time
	<-ch
	// the source learns the duration once decoding starts (local files)
	f.Current.Duration = 3 * time.Minute
	p.Tick(ctx)
	select {
	case s := <-ch:
		if s.NowPlaying == nil || s.NowPlaying.Track.Duration != 3*time.Minute {
			t.Fatalf("duration not propagated: %+v", s.NowPlaying)
		}
	default:
		t.Fatal("expected broadcast when duration becomes known")
	}
}

func TestMovePinsOnlyThatItem(t *testing.T) {
	p, _ := setup(t)
	ctx := context.Background()
	_ = p.Request(ctx, a, g1) // playing
	_ = p.Request(ctx, b, g1)
	time.Sleep(time.Millisecond)
	_ = p.Request(ctx, c, g2)
	ch, cancel := p.Subscribe()
	defer cancel()
	<-ch

	cID := p.State().Queue[1].ID
	if err := p.Move(cID, 0); err != nil {
		t.Fatal(err)
	}
	eq(t, queueIDs(p.State()), []string{"c", "b"})
	s := <-ch
	if s.Event == nil || s.Event.Type != "queue_moved" || s.Event.Title != "C" {
		t.Fatalf("event: %+v", s.Event)
	}
	if p.State().Event != nil {
		t.Fatal("event must be one-shot")
	}

	// the pinned item keeps its slot...
	bID := p.State().Queue[1].ID
	_ = p.Vote(bID, g2.ID)
	_ = p.Vote(bID, g3.ID)
	eq(t, queueIDs(p.State()), []string{"c", "b"})
	// ...while everything else still sorts by votes around it
	d := source.Track{ID: "d", Title: "D", Source: "fake"}
	e := source.Track{ID: "e", Title: "E", Source: "fake"}
	_ = p.Request(ctx, d, g1)
	_ = p.Request(ctx, e, g1)
	_ = p.Vote(p.State().Queue[3].ID, g2.ID) // e gets a vote
	eq(t, queueIDs(p.State()), []string{"c", "b", "e", "d"})
	dID := p.State().Queue[3].ID
	for _, g := range []Guest{g2, g3, {ID: "g4", Name: "Park"}} {
		_ = p.Vote(dID, g.ID) // with its requester's vote d has 4, b has 3
	}
	eq(t, queueIDs(p.State()), []string{"c", "d", "b", "e"})

	if err := p.Move("nope", 0); err != ErrNotInQueue {
		t.Fatal("unknown item")
	}
	_ = p.Move(cID, 99) // clamps to the end
	eq(t, queueIDs(p.State()), []string{"d", "b", "e", "c"})
}

func TestSeekBroadcastsEvent(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	if err := p.Seek(ctx, time.Minute); err == nil {
		t.Fatal("seek with nothing playing should fail")
	}
	_ = p.Request(ctx, a, g1)
	ch, cancel := p.Subscribe()
	defer cancel()
	<-ch
	if err := p.Seek(ctx, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if f.Position != 90*time.Second {
		t.Fatalf("source not seeked: %v", f.Position)
	}
	s := <-ch
	if s.Event == nil || s.Event.Type != "seek" || s.Event.Position != 90*time.Second || s.NowPlaying.Position != 90*time.Second {
		t.Fatalf("frame: %+v %+v", s.Event, s.NowPlaying)
	}
	// admin skip and admin removal announce too; guest self-removal does not
	_ = p.Request(ctx, b, g1)
	_ = p.Request(ctx, c, g2)
	<-ch
	<-ch
	_ = p.Remove(p.State().Queue[1].ID, g2.ID, false)
	if s := <-ch; s.Event != nil {
		t.Fatalf("guest removal should be silent: %+v", s.Event)
	}
	_ = p.Remove(p.State().Queue[0].ID, "", true)
	if s := <-ch; s.Event == nil || s.Event.Type != "queue_removed" || s.Event.Title != "B" {
		t.Fatalf("admin removal event: %+v", s.Event)
	}
	p.Skip(ctx)
	if s := <-ch; s.Event == nil || s.Event.Type != "skipped" || s.Event.Title != "A" {
		t.Fatalf("skip event: %+v", s.Event)
	}
}

func TestPersistAndRestore(t *testing.T) {
	p, _ := setup(t)
	ctx := context.Background()
	var saved [][]PersistedItem
	p.SetPersister(func(items []PersistedItem) { saved = append(saved, items) })

	_ = p.Request(ctx, a, g1) // plays immediately: queue stays empty, nothing to persist
	_ = p.Request(ctx, b, g1)
	_ = p.Request(ctx, c, g2)
	_ = p.Vote(p.State().Queue[1].ID, g1.ID)
	p.Tick(ctx) // no queue change: no extra save
	if len(saved) != 3 {
		t.Fatalf("saves = %d, want 3 (b, c, vote)", len(saved))
	}
	last := saved[len(saved)-1]
	if len(last) != 2 || last[0].Track.ID != "c" || len(last[0].Votes) != 2 || last[0].RequestedByName != "Lee" {
		t.Fatalf("persisted: %+v", last)
	}

	// a fresh player restores the same order and votes
	f2 := fake.New(a, b, c)
	p2 := New(Options{Sources: []source.Source{f2}})
	_ = p2.SetEnabled(ctx, "fake", true)
	p2.Restore(last)
	s := p2.State()
	eq(t, queueIDs(s), []string{"c", "b"})
	if s.Queue[0].Votes != 2 || s.Queue[0].RequestedByName != "Lee" {
		t.Fatalf("restored: %+v", s.Queue[0])
	}
	// and starts playing the head on the next tick
	p2.Tick(ctx)
	eq(t, f2.Played(), []string{"c"})
}

func TestOnPlayFires(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	var played []string
	p.SetOnPlay(func(src string, tr source.Track) { played = append(played, src+":"+tr.ID) })
	_ = p.Request(ctx, a, g1)
	_ = p.Request(ctx, b, g1)
	f.FinishTrack()
	p.Tick(ctx)
	eq(t, played, []string{"fake:a", "fake:b"})
}

func TestPlayFailureKeepsHeadQueued(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	f.Err = errors.New("player not ready")
	_ = p.Request(ctx, a, g1)
	f.Err = nil
	f.Calls = nil // Request logged a failed play; clear so Played() is clean
	eq(t, queueIDs(p.State()), []string{"a"})
	p.Tick(ctx)
	eq(t, f.Played(), []string{"a"})
	if len(p.State().Queue) != 0 {
		t.Fatal("head should be popped after a successful retry")
	}
}

func TestPositionDriftForcesFrame(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	_ = p.Request(ctx, a, g1)
	ch, cancel := p.Subscribe()
	defer cancel()
	<-ch
	f.Position = 500 * time.Millisecond
	p.Tick(ctx) // within tolerance: silent
	select {
	case s := <-ch:
		t.Fatalf("small drift should not broadcast: %+v", s.NowPlaying)
	default:
	}
	f.Position = 45 * time.Second // the host dragged the embedded player's own timeline
	p.Tick(ctx)
	select {
	case s := <-ch:
		if s.NowPlaying.Position != 45*time.Second {
			t.Fatalf("frame position = %v", s.NowPlaying.Position)
		}
	default:
		t.Fatal("large drift must force a frame")
	}
	// seek reports the requested position even though the fake's status is stale
	f.Position = 45 * time.Second
	if err := p.Seek(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if s := <-ch; s.NowPlaying.Position != 10*time.Second || s.Event == nil || s.Event.Type != "seek" {
		t.Fatalf("seek frame: %+v %+v", s.NowPlaying, s.Event)
	}
}

// A source that never searched for the queued track (most-played pick,
// restored queue) reports a bare Track; the queued metadata must survive.
func TestStatusKeepsQueuedMetadata(t *testing.T) {
	p, f := setup(t)
	full := source.Track{ID: "a", Title: "A", Artist: "Artist", ArtworkURL: "http://art/a.jpg", Source: "fake"}
	p.Request(context.Background(), full, g1)
	p.Tick(context.Background())
	f.Current = &source.Track{ID: "a", Source: "fake"}
	f.Playing = true
	p.Tick(context.Background())
	np := p.State().NowPlaying
	if np == nil || np.Track.Title != "A" || np.Track.ArtworkURL != "http://art/a.jpg" {
		t.Fatalf("metadata lost: %+v", np)
	}
}

// A passed vote waits SkipGrace so a guest can withdraw it; the skip then
// happens only if the votes still reach the threshold.
func TestSkipGraceCancellable(t *testing.T) {
	SkipGrace = 50 * time.Millisecond
	defer func() { SkipGrace = defaultSkipGrace }()
	p, f := setup(t)
	ctx := context.Background()
	_ = p.Request(ctx, a, g1)
	_ = p.Request(ctx, b, g1)
	p.SetConnectedGuests(0) // threshold 1
	if !p.VoteSkip(ctx, g1.ID) {
		t.Fatal("vote should pass")
	}
	if p.State().SkipAt == "" || !p.HasSkipVote(g1.ID) {
		t.Fatal("passed vote should start the grace period")
	}
	eq(t, f.Played(), []string{"a"})
	p.UnvoteSkip(g1.ID)
	if p.State().SkipAt != "" {
		t.Fatal("withdrawn vote should cancel the pending skip")
	}
	time.Sleep(3 * SkipGrace)
	eq(t, f.Played(), []string{"a"})
	_ = p.VoteSkip(ctx, g1.ID)
	time.Sleep(3 * SkipGrace)
	eq(t, f.Played(), []string{"a", "b"})
	if p.State().SkipAt != "" || p.State().SkipVotes != 0 {
		t.Fatal("grace and votes should clear after the skip")
	}
}
