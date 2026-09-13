// Package player is the core: it owns the queue and votes, drives whichever
// Source is active, and fans state out to subscribers (guest SSE, admin window).
package player

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/hwhang0917/vibe-music/internal/source"
)

// driftTolerance is how far the reported position may stray from what clients
// are interpolating before a frame is forced out (position is otherwise not
// part of the change signature).
const driftTolerance = 2 * time.Second

// playGrace is how long after Play() we ignore "nothing loaded" from Status():
// Spotify's player state lags a poll or two behind the command.
// ponytail: fixed grace; make it per-source if a backend needs more.
const playGrace = 3 * time.Second

// Guest is whoever is acting: a cookie ID plus the display name they chose.
type Guest struct {
	ID   string
	Name string
}

type QueueItem struct {
	ID              string       `json:"id"`
	Track           source.Track `json:"track"`
	RequestedBy     string       `json:"-"`
	RequestedByName string       `json:"requestedBy"`
	RequestedAt     time.Time    `json:"requestedAt"`
	Votes           int          `json:"votes"`
	// Mine is set per recipient by the server (the requester's cookie ID must not leak).
	Mine bool `json:"mine,omitempty"`
	// rank > 0 means the admin placed this item by hand; ranked items come
	// first in rank order and votes no longer move them. Unranked requests
	// below still sort by votes. ponytail: "admin curates the top, guests vote
	// on the rest"; a full manual mode can replace this if it confuses people.
	rank  int
	votes map[string]struct{}
}

// Event is a one-shot notice attached to a single state frame so UIs can
// toast what the host just did.
type Event struct {
	Type     string        `json:"type"` // "seek", "queue_moved", "queue_removed", "skipped"
	Title    string        `json:"title,omitempty"`
	Position time.Duration `json:"position,omitempty"`
}

type NowPlaying struct {
	Track       source.Track  `json:"track"`
	Playing     bool          `json:"playing"`
	Position    time.Duration `json:"position"`
	At          time.Time     `json:"at"`
	RequestedBy string        `json:"requestedBy,omitempty"`
}

type SourceInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	Exclusive  bool   `json:"exclusive"`  // when enabled, no other source may be
	HasChart   bool   `json:"hasChart"`   // implements source.Charter
	HasFolders bool   `json:"hasFolders"` // implements source.Explorer
}

// ExclusiveSources cannot share a queue with other sources (Spotify's policy
// forbids mixing its content with other audio).
var ExclusiveSources = map[string]bool{"spotify": true}

// SkipGrace is how long a passed skip vote waits before the track changes, so
// guests can withdraw a vote. A var so tests can shorten it.
const defaultSkipGrace = 3 * time.Second

var SkipGrace = defaultSkipGrace

// State is the snapshot guests and the admin window render.
type State struct {
	Sources       []SourceInfo `json:"sources"` // every source with its enabled flag
	NowPlaying    *NowPlaying  `json:"nowPlaying"`
	Queue         []QueueItem  `json:"queue"`
	SkipVotes     int          `json:"skipVotes"`
	SkipThreshold int          `json:"skipThreshold"`
	SkipAt        string       `json:"skipAt,omitempty"`     // RFC3339 deadline while a passed skip vote waits
	MySkipVote    bool         `json:"mySkipVote,omitempty"` // set per guest by the server
	Volume        int          `json:"volume"`
	Guests        int          `json:"guests"`
	Event         *Event       `json:"event,omitempty"`
}

type Options struct {
	Sources      []source.Source
	PollInterval time.Duration // default 1s
	SkipRatio    float64       // fraction of connected guests needed to skip, default 0.5
}

type Player struct {
	mu        sync.Mutex
	sources   map[string]source.Source
	order     []string
	enabled   map[string]bool
	current   source.Source // the one playing now
	queue     []*QueueItem
	now       source.Playback
	requester string // display name of who requested the current track
	skipVotes map[string]struct{}
	skipAt    time.Time   // deadline of a passed skip vote; zero when none pending
	skipTimer *time.Timer // fires skipNow at skipAt
	guests    int
	volume    int
	lastPlay  time.Time
	subs      map[chan State]struct{}
	lastSig   string
	pending   *Event // attached to the next broadcast, which is then forced
	force     bool   // broadcast even if the signature is unchanged (position drift)
	sentPos   time.Duration
	sentAt    time.Time
	sentPlay  bool
	persist   func([]PersistedItem)
	lastQueue string
	onPlay    func(sourceID string, t source.Track)
	poll      time.Duration
	skipRatio float64
}

func New(opts Options) *Player {
	p := &Player{
		sources:   map[string]source.Source{},
		enabled:   map[string]bool{},
		skipVotes: map[string]struct{}{},
		subs:      map[chan State]struct{}{},
		volume:    100,
		poll:      opts.PollInterval,
		skipRatio: opts.SkipRatio,
	}
	if p.poll == 0 {
		p.poll = time.Second
	}
	if p.skipRatio == 0 {
		p.skipRatio = 0.5
	}
	for _, s := range opts.Sources {
		p.sources[s.ID()] = s
		p.order = append(p.order, s.ID())
	}
	return p
}

// Run polls the active source until ctx is done.
func (p *Player) Run(ctx context.Context) {
	t := time.NewTicker(p.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Tick(ctx)
		}
	}
}

// Tick is one poll iteration. Status() runs outside the lock: for Spotify it
// is a network call and must not stall guest requests.
func (p *Player) Tick(ctx context.Context) {
	p.mu.Lock()
	src := p.current
	p.mu.Unlock()
	if src == nil {
		// nothing has played yet (or the last source was switched off): start the head
		p.mu.Lock()
		if p.current == nil && len(p.queue) > 0 && time.Since(p.lastPlay) > playGrace {
			p.advance(ctx)
		}
		p.broadcastIfChanged()
		p.mu.Unlock()
		return
	}
	st, err := src.Status(ctx)
	if err != nil {
		log.Println("player: status:", err)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current != src {
		return // switched while we were polling
	}
	p.applyStatus(st)
	if st.Track != nil && !p.sentAt.IsZero() {
		expected := p.sentPos
		if p.sentPlay {
			expected += st.At.Sub(p.sentAt)
		}
		if d := st.Position - expected; d > driftTolerance || d < -driftTolerance {
			p.force = true
		}
	}
	idle := st.Track == nil && !st.Playing && time.Since(p.lastPlay) > playGrace
	if st.Ended || (idle && len(p.queue) > 0) {
		p.advance(ctx)
	}
	p.broadcastIfChanged()
}

// Sources lists every source with its enabled flag, in registration order.
func (p *Player) Sources() []SourceInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sourcesLocked()
}

func (p *Player) sourcesLocked() []SourceInfo {
	out := make([]SourceInfo, 0, len(p.order))
	for _, id := range p.order {
		_, hasChart := p.sources[id].(source.Charter)
		_, hasFolders := p.sources[id].(source.Explorer)
		out = append(out, SourceInfo{ID: id, Name: p.sources[id].Name(), Enabled: p.enabled[id], Exclusive: ExclusiveSources[id], HasChart: hasChart, HasFolders: hasFolders})
	}
	return out
}

// Source returns a registered source by ID (enabled or not).
func (p *Player) Source(id string) (source.Source, bool) {
	s, ok := p.sources[id]
	return s, ok
}

// Enabled reports whether guests may search and queue from a source.
func (p *Player) Enabled(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enabled[id]
}

// EnabledIDs returns the enabled sources in registration order.
func (p *Player) EnabledIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, id := range p.order {
		if p.enabled[id] {
			out = append(out, id)
		}
	}
	return out
}

// ErrExclusive is returned when enabling would put an exclusive source next to others.
var ErrExclusive = &source.CodedError{Kind: "source_exclusive", Msg: "an exclusive source cannot be enabled alongside others"}

// SetEnabled switches a source on (Activate) or off. Switching off stops it if
// it is playing, drops its queued items, and announces the change. Exclusive
// rules are enforced here; the admin UI turns the others off first.
func (p *Player) SetEnabled(ctx context.Context, id string, on bool) error {
	src, ok := p.sources[id]
	if !ok {
		return fmt.Errorf("unknown_source: %s", id)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if on {
		if p.enabled[id] {
			return nil
		}
		for other, en := range p.enabled {
			if en && (ExclusiveSources[id] || ExclusiveSources[other]) {
				return ErrExclusive
			}
		}
		if err := src.Activate(ctx); err != nil {
			return err
		}
		_ = src.SetVolume(ctx, p.volume)
		p.enabled[id] = true
		p.broadcastIfChanged()
		return nil
	}
	if !p.enabled[id] {
		return nil
	}
	delete(p.enabled, id)
	kept := p.queue[:0]
	for _, it := range p.queue {
		if it.Track.Source != id {
			kept = append(kept, it)
		}
	}
	p.queue = kept
	if p.current == src {
		p.current = nil
		p.now = source.Playback{}
		p.requester = ""
		p.resetSkip()
		p.lastPlay = time.Time{} // nothing is loading: the next tick may start the new head at once
	}
	if err := src.Deactivate(ctx); err != nil {
		log.Println("player: deactivate:", err)
	}
	p.pending = &Event{Type: "source_disabled", Title: src.Name()}
	p.broadcastIfChanged()
	return nil
}

// ErrNoSource means the admin has not activated any source.
var ErrNoSource = &source.CodedError{Kind: "no_source", Msg: "no active source"}

// Search queries one enabled source. An empty sourceID means the only enabled
// one; with several enabled the caller must pick.
func (p *Player) Search(ctx context.Context, sourceID, q string, limit int) ([]source.Track, error) {
	p.mu.Lock()
	if sourceID == "" {
		var ids []string
		for _, id := range p.order {
			if p.enabled[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) == 1 {
			sourceID = ids[0]
		}
	}
	src, ok := p.sources[sourceID]
	enabled := p.enabled[sourceID]
	p.mu.Unlock()
	if !ok || !enabled {
		return nil, ErrNoSource
	}
	return src.Search(ctx, q, limit)
}

// Chart lists a source's popularity chart, or nothing if it has none.
func (p *Player) Chart(ctx context.Context, sourceID, region string, limit int) ([]source.Track, error) {
	p.mu.Lock()
	src, ok := p.sources[sourceID]
	enabled := p.enabled[sourceID]
	p.mu.Unlock()
	if !ok || !enabled {
		return nil, ErrNoSource
	}
	ch, ok := src.(source.Charter)
	if !ok {
		return []source.Track{}, nil
	}
	return ch.Chart(ctx, region, limit)
}

// browser returns the enabled source as a Browser, or why it cannot browse.
func (p *Player) browser(sourceID string) (source.Browser, error) {
	p.mu.Lock()
	src, ok := p.sources[sourceID]
	enabled := p.enabled[sourceID]
	p.mu.Unlock()
	if !ok || !enabled {
		return nil, ErrNoSource
	}
	b, ok := src.(source.Browser)
	if !ok {
		return nil, source.ErrNotFound
	}
	return b, nil
}

// Folder lists one folder of a source laid out as files ("" = top level).
func (p *Player) Folder(ctx context.Context, sourceID, id string) (source.Folder, error) {
	p.mu.Lock()
	src, ok := p.sources[sourceID]
	enabled := p.enabled[sourceID]
	p.mu.Unlock()
	if !ok || !enabled {
		return source.Folder{}, ErrNoSource
	}
	e, ok := src.(source.Explorer)
	if !ok {
		return source.Folder{}, source.ErrNotFound
	}
	return e.Folder(ctx, id)
}

// Artist shows a source's artist page.
func (p *Player) Artist(ctx context.Context, sourceID, id string) (source.Artist, error) {
	b, err := p.browser(sourceID)
	if err != nil {
		return source.Artist{}, err
	}
	return b.Artist(ctx, id)
}

// Album shows a source's album page.
func (p *Player) Album(ctx context.Context, sourceID, id string) (source.Album, error) {
	b, err := p.browser(sourceID)
	if err != nil {
		return source.Album{}, err
	}
	return b.Album(ctx, id)
}

// Request queues a track. The same song may be queued more than once; each
// request is its own entry. If nothing is playing the track starts right away.
func (p *Player) Request(ctx context.Context, t source.Track, g Guest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled[t.Source] {
		return ErrNoSource
	}
	p.queue = append(p.queue, &QueueItem{
		ID:              newID(),
		Track:           t,
		RequestedBy:     g.ID,
		RequestedByName: g.Name,
		RequestedAt:     time.Now(),
		votes:           map[string]struct{}{g.ID: {}},
	})
	p.sortQueue()
	if p.now.Track == nil && !p.now.Playing && time.Since(p.lastPlay) > playGrace {
		p.advance(ctx)
	}
	p.broadcastIfChanged()
	return nil
}

func (p *Player) Vote(itemID, guestID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, it := range p.queue {
		if it.ID == itemID {
			it.votes[guestID] = struct{}{}
			p.sortQueue()
			p.broadcastIfChanged()
			return nil
		}
	}
	return ErrNotInQueue
}

// ErrNotOwner is returned when a guest tries to remove someone else's request.
var ErrNotOwner = errors.New("not your request")

// ErrNotInQueue is returned for unknown queue items.
var ErrNotInQueue = errors.New("not in queue")

// Remove drops a queued item. Guests may only remove their own requests;
// admin passes admin=true to remove anything.
func (p *Player) Remove(itemID, guestID string, admin bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, it := range p.queue {
		if it.ID != itemID {
			continue
		}
		if !admin && it.RequestedBy != guestID {
			return ErrNotOwner
		}
		p.queue = append(p.queue[:i], p.queue[i+1:]...)
		if admin {
			p.pending = &Event{Type: "queue_removed", Title: it.Track.Title}
		}
		p.broadcastIfChanged()
		return nil
	}
	return ErrNotInQueue
}

// VoteSkip records a skip vote and advances when the threshold is met.
// Returns whether the track was skipped.
func (p *Player) VoteSkip(ctx context.Context, guestID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.now.Track == nil {
		return false
	}
	p.skipVotes[guestID] = struct{}{}
	passed := len(p.skipVotes) >= p.skipThreshold()
	if passed && p.skipTimer == nil {
		p.skipAt = time.Now().Add(SkipGrace)
		p.skipTimer = time.AfterFunc(SkipGrace, p.skipNow)
	}
	p.broadcastIfChanged()
	return passed
}

// skipNow ends the grace period; votes may have been withdrawn meanwhile.
func (p *Player) skipNow() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.skipTimer == nil {
		return // cancelled
	}
	p.skipTimer, p.skipAt = nil, time.Time{}
	if len(p.skipVotes) >= p.skipThreshold() {
		p.advance(context.Background())
	}
	p.broadcastIfChanged()
}

// UnvoteSkip withdraws a guest's skip vote; a pending skip is cancelled once
// the count drops below the threshold.
func (p *Player) UnvoteSkip(guestID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.skipVotes, guestID)
	p.cancelSkipIfShort()
	p.broadcastIfChanged()
}

func (p *Player) HasSkipVote(guestID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.skipVotes[guestID]
	return ok
}

func (p *Player) cancelSkipIfShort() {
	if p.skipTimer != nil && len(p.skipVotes) < p.skipThreshold() {
		p.skipTimer.Stop()
		p.skipTimer, p.skipAt = nil, time.Time{}
	}
}

// resetSkip forgets votes and any pending skip when the track changes.
func (p *Player) resetSkip() {
	p.skipVotes = map[string]struct{}{}
	if p.skipTimer != nil {
		p.skipTimer.Stop()
	}
	p.skipTimer, p.skipAt = nil, time.Time{}
}

// Move places a queued item at index (0 = next up). All current items become
// admin-ranked in their resulting order; see QueueItem.rank.
func (p *Player) Move(itemID string, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	from := -1
	for i, it := range p.queue {
		if it.ID == itemID {
			from = i
		}
	}
	if from < 0 {
		return ErrNotInQueue
	}
	index = max(0, min(len(p.queue)-1, index))
	it := p.queue[from]
	rest := append(append([]*QueueItem{}, p.queue[:from]...), p.queue[from+1:]...)
	p.queue = append(append(append([]*QueueItem{}, rest[:index]...), it), rest[index:]...)
	for i, q := range p.queue {
		q.rank = i + 1
	}
	p.pending = &Event{Type: "queue_moved", Title: it.Track.Title}
	p.broadcastIfChanged()
	return nil
}

// Seek moves the current track to pos and tells everyone.
func (p *Player) Seek(ctx context.Context, pos time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil || p.now.Track == nil {
		return errors.New("nothing playing")
	}
	if err := p.current.Seek(ctx, pos); err != nil {
		return err
	}
	// Sources report asynchronously (YouTube, Spotify); their status right now
	// is the pre-seek one. Report the requested position and let the next
	// poll refine it.
	p.now.Position, p.now.At = pos, time.Now()
	p.pending = &Event{Type: "seek", Position: pos}
	p.broadcastIfChanged()
	return nil
}

// Skip is the admin override.
func (p *Player) Skip(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.now.Track != nil {
		p.pending = &Event{Type: "skipped", Title: p.now.Track.Title}
	}
	p.advance(ctx)
	p.broadcastIfChanged()
}

func (p *Player) Pause(ctx context.Context) error {
	return p.control(func(s source.Source) error { return s.Pause(ctx) })
}

func (p *Player) Resume(ctx context.Context) error {
	return p.control(func(s source.Source) error { return s.Resume(ctx) })
}

func (p *Player) SetVolume(ctx context.Context, pct int) error {
	pct = max(0, min(100, pct))
	p.mu.Lock()
	defer p.mu.Unlock()
	p.volume = pct
	var err error
	for id, on := range p.enabled {
		if on {
			if e := p.sources[id].SetVolume(ctx, pct); e != nil {
				err = e
			}
		}
	}
	p.broadcastIfChanged()
	return err
}

func (p *Player) control(fn func(source.Source) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return errors.New("nothing playing")
	}
	err := fn(p.current)
	if st, serr := p.current.Status(context.Background()); serr == nil {
		p.applyStatus(st)
	}
	p.broadcastIfChanged()
	return err
}

// RemoveGuest drops everything a guest contributed: their queued requests,
// their upvotes, and their skip vote. Used when the admin removes or blocks them.
func (p *Player) RemoveGuest(guestID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.queue[:0]
	for _, it := range p.queue {
		delete(it.votes, guestID)
		if it.RequestedBy != guestID {
			kept = append(kept, it)
	p.cancelSkipIfShort()
		}
	}
	p.queue = kept
	delete(p.skipVotes, guestID)
	p.sortQueue()
	p.broadcastIfChanged()
}

// SetSkipRatio changes the fraction of connected guests needed to skip.
func (p *Player) SetSkipRatio(r float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r > 0 && r <= 1 {
		p.skipRatio = r
	}
	p.broadcastIfChanged()
}

// SetConnectedGuests updates the denominator for the skip threshold.
func (p *Player) SetConnectedGuests(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.guests = n
	p.broadcastIfChanged()
}

func (p *Player) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshot()
}

// PersistedItem is a queue entry as stored across relaunches.
type PersistedItem struct {
	ID              string
	Track           source.Track
	RequestedBy     string
	RequestedByName string
	RequestedAt     time.Time
	Rank            int
	Votes           []string
}

// SetPersister registers a callback that receives the whole queue whenever it
// changes. It runs under the player lock, so it must be quick (a local
// SQLite write is). ponytail: synchronous; hand it a channel if it ever blocks.
func (p *Player) SetPersister(fn func([]PersistedItem)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.persist = fn
}

// SetOnPlay registers a callback fired whenever a track starts (play history).
// Runs under the player lock; keep it quick.
func (p *Player) SetOnPlay(fn func(sourceID string, t source.Track)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onPlay = fn
}

// Restore loads a previously persisted queue (at startup, before Run).
func (p *Player) Restore(items []PersistedItem) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = p.queue[:0]
	for _, it := range items {
		votes := make(map[string]struct{}, len(it.Votes))
		for _, v := range it.Votes {
			votes[v] = struct{}{}
		}
		p.queue = append(p.queue, &QueueItem{
			ID: it.ID, Track: it.Track, RequestedBy: it.RequestedBy, RequestedByName: it.RequestedByName,
			RequestedAt: it.RequestedAt, rank: it.Rank, votes: votes,
		})
	}
	p.sortQueue()
	p.lastQueue = p.queueSig()
	p.broadcastIfChanged()
}

func (p *Player) persisted() []PersistedItem {
	out := make([]PersistedItem, 0, len(p.queue))
	for _, it := range p.queue {
		votes := make([]string, 0, len(it.votes))
		for v := range it.votes {
			votes = append(votes, v)
		}
		sort.Strings(votes)
		out = append(out, PersistedItem{
			ID: it.ID, Track: it.Track, RequestedBy: it.RequestedBy, RequestedByName: it.RequestedByName,
			RequestedAt: it.RequestedAt, Rank: it.rank, Votes: votes,
		})
	}
	return out
}

func (p *Player) queueSig() string {
	sig := ""
	for _, it := range p.queue {
		sig += fmt.Sprintf("%s:%d:%d,", it.ID, it.rank, len(it.votes))
	}
	return sig
}

// Subscribe returns a channel receiving every state change. Slow readers drop
// frames rather than block the player.
func (p *Player) Subscribe() (<-chan State, func()) {
	ch := make(chan State, 8)
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	ch <- p.snapshot()
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}
}

// --- internals (caller holds p.mu) ---

// applyStatus adopts the source's snapshot but keeps the metadata the track was
// queued with. A source only gets an ID on Play, so one that never searched for
// that track this session (a most-played pick, a restored queue) answers with a
// bare Track and the artwork and title would vanish once playback starts.
func (p *Player) applyStatus(st source.Playback) {
	if st.Track != nil && p.now.Track != nil && st.Track.ID == p.now.Track.ID {
		q, t := *p.now.Track, *st.Track
		if t.Title == "" {
			t.Title = q.Title
		}
		if t.Artist == "" {
			t.Artist = q.Artist
		}
		if t.Album == "" {
			t.Album = q.Album
		}
		if t.ArtworkURL == "" {
			t.ArtworkURL = q.ArtworkURL
		}
		if t.ExternalURL == "" {
			t.ExternalURL = q.ExternalURL
		}
		if t.Duration == 0 {
			t.Duration = q.Duration
		}
		st.Track = &t
	}
	p.now = st
}

// advance plays the queue head (switching source if the head lives elsewhere)
// or stops when the queue is empty.
func (p *Player) advance(ctx context.Context) {
	p.resetSkip()
	if len(p.queue) == 0 {
		if p.current != nil {
			_ = p.current.Stop(ctx)
		}
		p.now = source.Playback{At: time.Now()}
		p.requester = ""
		return
	}
	head := p.queue[0]
	next, ok := p.sources[head.Track.Source]
	if !ok || !p.enabled[head.Track.Source] {
		p.queue = p.queue[1:] // orphaned by a disable race; drop it
		return
	}
	if p.current != nil && p.current != next {
		_ = p.current.Stop(ctx) // never overlap two sources
	}
	if err := next.Play(ctx, head.Track.ID); err != nil {
		log.Println("player: play:", err) // head stays queued; the next tick retries
		return
	}
	p.current = next
	p.queue = p.queue[1:]
	p.lastPlay = time.Now()
	p.requester = head.RequestedByName
	p.now = source.Playback{Track: &head.Track, Playing: true, At: p.lastPlay}
	if p.onPlay != nil {
		p.onPlay(next.ID(), head.Track)
	}
}

func (p *Player) sortQueue() {
	sort.SliceStable(p.queue, func(i, j int) bool {
		a, b := p.queue[i], p.queue[j]
		if (a.rank > 0) != (b.rank > 0) {
			return a.rank > 0 // admin-ranked first
		}
		if a.rank > 0 {
			return a.rank < b.rank
		}
		if va, vb := len(a.votes), len(b.votes); va != vb {
			return va > vb
		}
		return a.RequestedAt.Before(b.RequestedAt)
	})
}

func (p *Player) skipThreshold() int {
	return max(1, int(math.Ceil(p.skipRatio*float64(p.guests))))
}

func (p *Player) snapshot() State {
	s := State{
		Queue:         make([]QueueItem, 0, len(p.queue)),
		SkipVotes:     len(p.skipVotes),
		SkipThreshold: p.skipThreshold(),
		Volume:        p.volume,
		Guests:        p.guests,
	}
	s.Sources = p.sourcesLocked()
	if p.now.Track != nil {
		s.NowPlaying = &NowPlaying{
			Track:       *p.now.Track,
			Playing:     p.now.Playing,
			Position:    p.now.Position,
			At:          p.now.At,
			RequestedBy: p.requester,
		}
	}
	for _, it := range p.queue {
		c := *it
		c.Votes = len(it.votes)
		s.Queue = append(s.Queue, c)
	if !p.skipAt.IsZero() {
		s.SkipAt = p.skipAt.UTC().Format(time.RFC3339Nano)
	}
	}
	return s
}

// signature excludes Position so a playing track does not broadcast every
// tick; clients interpolate from Position+At of the last frame. Duration is
// included because local files only learn it once decoding starts.
func (s State) signature() string {
	sig := fmt.Sprintf("%v|%d|%d|%s|%d|%d|", s.Sources, s.SkipVotes, s.SkipThreshold, s.SkipAt, s.Volume, s.Guests)
	if s.NowPlaying != nil {
		sig += fmt.Sprintf("%s|%v|%d|", s.NowPlaying.Track.ID, s.NowPlaying.Playing, s.NowPlaying.Track.Duration)
	}
	for _, it := range s.Queue {
		sig += fmt.Sprintf("%s:%d,", it.ID, it.Votes)
	}
	return sig
}

// Announce attaches a one-shot Event to the next state frame (and sends it).
func (p *Player) Announce(ev Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = &ev
	p.broadcastIfChanged()
}

func (p *Player) broadcastIfChanged() {
	if q := p.queueSig(); q != p.lastQueue {
		p.lastQueue = q
		if p.persist != nil {
			p.persist(p.persisted())
		}
	}
	s := p.snapshot()
	sig := s.signature()
	if sig == p.lastSig && p.pending == nil && !p.force {
		return
	}
	p.lastSig = sig
	p.force = false
	s.Event, p.pending = p.pending, nil
	if s.NowPlaying != nil {
		p.sentPos, p.sentAt, p.sentPlay = s.NowPlaying.Position, s.NowPlaying.At, s.NowPlaying.Playing
	} else {
		p.sentAt = time.Time{}
	}
	for ch := range p.subs {
		select {
		case ch <- s:
		default:
		}
	}
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}
