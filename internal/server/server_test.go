package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hwhang0917/vibe-music/internal/store"

	"github.com/hwhang0917/vibe-music/internal/player"
	"github.com/hwhang0917/vibe-music/internal/source"
	"github.com/hwhang0917/vibe-music/internal/source/fake"
)

var dist = fstest.MapFS{
	"index.html":    {Data: []byte("<html>guest</html>")},
	"assets/app.js": {Data: []byte("console.log(1)")},
}

func newGuests(t *testing.T, onChange func()) *Guests {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "vibe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewGuests(db, false, onChange)
}

func newTestServer(t *testing.T) (*httptest.Server, *fake.Source, *Guests) {
	t.Helper()
	f := fake.New(
		source.Track{ID: "a", Title: "Alpha", Artist: "X"},
		source.Track{ID: "b", Title: "Beta", Artist: "Y"},
	)
	p := player.New(player.Options{Sources: []source.Source{f}})
	if err := p.SetEnabled(context.Background(), "fake", true); err != nil {
		t.Fatal(err)
	}
	guests := newGuests(t, nil)
	top := func(src string, limit int) ([]source.Track, error) {
		if src != "fake" {
			t.Fatalf("top called for %q", src)
		}
		return []source.Track{{ID: "a", Title: "Alpha"}}, nil
	}
	ts := httptest.NewServer(NewHandler(dist, p, guests, top, nil))
	t.Cleanup(ts.Close)
	return ts, f, guests
}

// client is a guest with a cookie jar.
type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func newClient(t *testing.T, base string) *client {
	c := &http.Client{}
	c.Jar = newJar()
	return &client{t: t, base: base, http: c}
}

func (c *client) do(method, p string, body string) (*http.Response, map[string]any) {
	req, _ := http.NewRequest(method, c.base+p, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func TestStaticAndSPA(t *testing.T) {
	ts, _, guests := newTestServer(t)
	c := newClient(t, ts.URL)
	for _, p := range []string{"/", "/vote/abc"} {
		res, _ := c.do("GET", p, "")
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d", p, res.StatusCode)
		}
	}
	res, _ := c.do("GET", "/assets/app.js", "")
	if res.StatusCode != 200 {
		t.Fatalf("static: %d", res.StatusCode)
	}
	rec := httptest.NewRecorder()
	NewHandler(fstest.MapFS{}, nil, guests, nil, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unbuilt UI: %d", rec.Code)
	}
}

func TestGuestFlow(t *testing.T) {
	ts, f, _ := newTestServer(t)
	kim := newClient(t, ts.URL)
	lee := newClient(t, ts.URL)

	// cookie issued on first call; write actions need a name first
	res, me := kim.do("GET", "/api/me", "")
	if res.StatusCode != 200 || me["id"] == "" || me["name"] != "" {
		t.Fatalf("me: %d %v", res.StatusCode, me)
	}
	res, _ = kim.do("POST", "/api/queue", `{"id":"a","source":"fake"}`)
	if res.StatusCode != 400 {
		t.Fatalf("request without name: %d", res.StatusCode)
	}
	res, _ = kim.do("POST", "/api/me", `{"name":"   "}`)
	if res.StatusCode != 400 {
		t.Fatalf("blank name: %d", res.StatusCode)
	}
	kim.do("POST", "/api/me", `{"name":"Kim"}`)
	lee.do("POST", "/api/me", `{"name":"Lee"}`)

	// most played comes from the history hook, scoped to the active source
	req, _ := http.NewRequest("GET", ts.URL+"/api/top", nil)
	res, err := kim.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var top []map[string]any
	json.NewDecoder(res.Body).Decode(&top)
	res.Body.Close()
	if res.StatusCode != 200 || len(top) != 1 || top[0]["title"] != "Alpha" {
		t.Fatalf("top: %d %v", res.StatusCode, top)
	}

	// search goes through the player, never straight to the source
	res, _ = kim.do("GET", "/api/search?q=alp", "")
	if r, out := kim.do("GET", "/api/search?q=alp&source=nope", ""); r.StatusCode != 502 || out["error"] != "no_source" {
		t.Fatalf("unknown source: %d %v", r.StatusCode, out)
	}
	if r, out := kim.do("POST", "/api/queue", `{"id":"a","source":"nope"}`); r.StatusCode != 409 || out["error"] != "no_source" {
		t.Fatalf("request for a disabled source: %d %v", r.StatusCode, out)
	}
	if res.StatusCode != 200 || f.Calls[len(f.Calls)-1] != "search:alp" {
		t.Fatalf("search: %d %v", res.StatusCode, f.Calls)
	}

	// first request plays, second queues with requester name
	kim.do("POST", "/api/queue", `{"id":"a","title":"Alpha","source":"fake"}`)
	res, st := lee.do("POST", "/api/queue", `{"id":"b","title":"Beta","source":"fake"}`)
	if res.StatusCode != 200 {
		t.Fatalf("request: %d", res.StatusCode)
	}
	queue := st["queue"].([]any)
	if len(queue) != 1 || queue[0].(map[string]any)["requestedBy"] != "Lee" {
		t.Fatalf("queue: %v", queue)
	}
	np := st["nowPlaying"].(map[string]any)
	if np["requestedBy"] != "Kim" {
		t.Fatalf("now playing: %v", np)
	}

	// "mine" is per recipient: Lee sees their request as theirs, Kim does not
	if queue[0].(map[string]any)["mine"] != true {
		t.Fatalf("lee should see mine=true: %v", queue[0])
	}
	_, kimView := kim.do("GET", "/api/state", "")
	if kimView["queue"].([]any)[0].(map[string]any)["mine"] == true {
		t.Fatal("kim should not see lee's request as mine")
	}

	// removal: only the owner or the admin
	itemID := queue[0].(map[string]any)["id"].(string)
	if res, out := kim.do("DELETE", "/api/queue/"+itemID, ""); res.StatusCode != 403 || out["error"] != "not_owner" {
		t.Fatalf("kim removing lee's request: %d %v", res.StatusCode, out)
	}
	if res, _ := lee.do("DELETE", "/api/queue/nope", ""); res.StatusCode != 404 {
		t.Fatalf("remove unknown: %d", res.StatusCode)
	}
	if res, st := lee.do("DELETE", "/api/queue/"+itemID, ""); res.StatusCode != 200 || len(st["queue"].([]any)) != 0 {
		t.Fatalf("lee removing own: %d %v", res.StatusCode, st["queue"])
	}
	_, st = lee.do("POST", "/api/queue", `{"id":"b","title":"Beta","source":"fake"}`)
	queue = st["queue"].([]any)
	itemID = queue[0].(map[string]any)["id"].(string)

	// vote on the queued item
	res, st = kim.do("POST", "/api/queue/"+itemID+"/vote", "")
	if res.StatusCode != 200 || st["queue"].([]any)[0].(map[string]any)["votes"].(float64) != 2 {
		t.Fatalf("vote: %d %v", res.StatusCode, st["queue"])
	}
	res, _ = kim.do("POST", "/api/queue/nope/vote", "")
	if res.StatusCode != 404 {
		t.Fatalf("vote unknown: %d", res.StatusCode)
	}

	// skip: no guests connected -> threshold 1; a passed vote waits SkipGrace
	// and can be withdrawn meanwhile
	player.SkipGrace = 20 * time.Millisecond
	defer func() { player.SkipGrace = 3 * time.Second }()
	res, out := lee.do("POST", "/api/skip", "")
	if res.StatusCode != 200 || out["skipped"] != true || out["state"].(map[string]any)["skipAt"] == nil {
		t.Fatalf("skip: %d %v", res.StatusCode, out)
	}
	res, out = lee.do("DELETE", "/api/skip", "")
	if res.StatusCode != 200 || out["state"].(map[string]any)["skipAt"] != nil {
		t.Fatalf("unskip: %d %v", res.StatusCode, out)
	}
	time.Sleep(3 * player.SkipGrace)
	if got := f.Played(); len(got) != 1 {
		t.Fatalf("withdrawn vote must not skip: %v", got)
	}
	res, _ = lee.do("POST", "/api/skip", "")
	time.Sleep(3 * player.SkipGrace)
	if got := f.Played(); len(got) != 2 || got[1] != "b" {
		t.Fatalf("played: %v", got)
	}

	// artwork: fake is not an ArtworkProvider
	res, _ = kim.do("GET", "/api/artwork/fake/a", "")
	if res.StatusCode != 404 {
		t.Fatalf("artwork: %d", res.StatusCode)
	}
}

func TestEventsCountsDistinctGuests(t *testing.T) {
	ts, _, _ := newTestServer(t)
	kim := newClient(t, ts.URL)
	kim.do("GET", "/api/me", "") // get a cookie

	open := func() (*http.Response, *bufio.Reader) {
		req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
		res, err := kim.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res, bufio.NewReader(res.Body)
	}
	readEvent := func(r *bufio.Reader) map[string]any {
		var data string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
			if line == "\n" && data != "" {
				var st map[string]any
				json.Unmarshal([]byte(data), &st)
				return st
			}
		}
	}

	res1, r1 := open()
	defer res1.Body.Close()
	if ct := res1.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type: %s", ct)
	}
	first := readEvent(r1) // initial snapshot
	if _, ok := first["queue"]; !ok {
		t.Fatalf("first event should be a state snapshot: %v", first)
	}

	// second tab, same guest: still 1 connected guest
	res2, _ := open()
	defer res2.Body.Close()
	_, st := kim.do("GET", "/api/state", "")
	if st["guests"].(float64) != 1 {
		t.Fatalf("guests = %v, want 1 for two tabs of one guest", st["guests"])
	}

	lee := newClient(t, ts.URL)
	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	res3, err := lee.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res3.Body.Close()
	_, st = kim.do("GET", "/api/state", "")
	if st["guests"].(float64) != 2 {
		t.Fatalf("guests = %v, want 2", st["guests"])
	}
}

func TestBlockAndKick(t *testing.T) {
	changes := 0
	guests := newGuests(t, func() { changes++ })
	f := fake.New(source.Track{ID: "a", Title: "Alpha"})
	p := player.New(player.Options{Sources: []source.Source{f}})
	p.SetEnabled(context.Background(), "fake", true)
	ts := httptest.NewServer(NewHandler(dist, p, guests, nil, nil))
	defer ts.Close()

	kim := newClient(t, ts.URL)
	_, me := kim.do("POST", "/api/me", `{"name":"Kim"}`)
	id := me["id"].(string)
	if list, _ := guests.List(); len(list) != 1 || list[0].Name != "Kim" || list[0].ID != id {
		t.Fatalf("list: %+v", list)
	}
	// the id handed out is a hash, never the cookie itself
	for _, c := range kim.http.Jar.Cookies(nil) {
		if c.Value == id {
			t.Fatal("guest id must not equal the cookie")
		}
	}

	// open a stream, then kick: the stream ends
	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	res, err := kim.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	waitFor(t, func() bool { return guests.Connected() == 1 })
	guests.Kick(id)
	buf := make([]byte, 4096)
	for {
		if _, err := res.Body.Read(buf); err != nil {
			break // EOF: server closed the stream
		}
	}
	waitFor(t, func() bool { return guests.Connected() == 0 })

	// block: every API call is 403 with a code the UI understands
	guests.SetBlocked(id, true)
	r, out := kim.do("POST", "/api/queue", `{"id":"a","source":"fake"}`)
	if r.StatusCode != 403 || out["error"] != "blocked" {
		t.Fatalf("blocked: %d %v", r.StatusCode, out)
	}
	if list, _ := guests.List(); len(list) != 1 || !list[0].Blocked {
		t.Fatalf("blocked list: %v", list)
	}
	guests.SetBlocked(id, false)
	if r, _ := kim.do("GET", "/api/me", ""); r.StatusCode != 200 {
		t.Fatalf("unblocked: %d", r.StatusCode)
	}

	// remove: name is forgotten
	guests.Remove(id)
	if _, me := kim.do("GET", "/api/me", ""); me["name"] != "" {
		t.Fatalf("name should be forgotten: %v", me)
	}
	if changes == 0 {
		t.Fatal("onChange never fired")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestInviteOnly(t *testing.T) {
	ts, _, guests := newTestServer(t)
	// no redirects: we want to see the 302s
	noRedirect := func(c *client) {
		c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}

	kim := newClient(t, ts.URL)
	kim.do("POST", "/api/me", `{"name":"Kim"}`) // already here before the switch

	if err := guests.SetInviteOnly(true); err != nil {
		t.Fatal(err)
	}
	if res, _ := kim.do("GET", "/api/me", ""); res.StatusCode != 200 {
		t.Fatalf("existing guest should stay admitted: %d", res.StatusCode)
	}

	lee := newClient(t, ts.URL)
	noRedirect(lee)
	if res, out := lee.do("GET", "/api/me", ""); res.StatusCode != 403 || out["error"] != "invite_required" {
		t.Fatalf("newcomer: %d %v", res.StatusCode, out)
	}
	if res, out := lee.do("POST", "/api/join", `{"code":"NOPE-NOPE"}`); res.StatusCode != 403 || out["error"] != "invite_invalid" {
		t.Fatalf("bad code: %d %v", res.StatusCode, out)
	}

	inv, err := guests.CreateInvitation(time.Hour)
	if err != nil || inv.Code == "" || inv.Label != inv.Code[len(inv.Code)-4:] {
		t.Fatalf("create: %+v %v", inv, err)
	}
	if res, _ := lee.do("POST", "/api/join", `{"code":"`+inv.Code+`"}`); res.StatusCode != 200 {
		t.Fatalf("good code: %d", res.StatusCode)
	}
	if res, _ := lee.do("GET", "/api/me", ""); res.StatusCode != 200 {
		t.Fatalf("admitted guest: %d", res.StatusCode)
	}
	list, _ := guests.Invitations()
	if len(list) != 1 || list[0].Uses != 1 || list[0].Code != inv.Code {
		t.Fatalf("invitations: %+v", list)
	}
	// a link works once: the guest who used it may reopen it, a second guest may not
	if res, _ := lee.do("POST", "/api/join", `{"code":"`+inv.Code+`"}`); res.StatusCode != 200 {
		t.Fatalf("same guest reopening: %d", res.StatusCode)
	}
	choi := newClient(t, ts.URL)
	if res, out := choi.do("POST", "/api/join", `{"code":"`+inv.Code+`"}`); res.StatusCode != 403 || out["error"] != "invite_invalid" {
		t.Fatalf("second guest on a used link: %d %v", res.StatusCode, out)
	}

	// revoked codes stop working; admin can still admit by hand
	guests.RevokeInvitation(inv.ID)
	park := newClient(t, ts.URL)
	noRedirect(park)
	if res, _ := park.do("POST", "/api/join", `{"code":"`+inv.Code+`"}`); res.StatusCode != 403 {
		t.Fatal("revoked code should be invalid")
	}
	_, me := park.do("GET", "/api/me", "") // 403, but the guest is now known
	_ = me
	gl, _ := guests.List()
	var pending []string
	for _, g := range gl {
		if !g.Admitted {
			pending = append(pending, g.ID)
		}
	}
	if len(pending) != 2 { // park and choi
		t.Fatalf("park and choi should be listed as not admitted: %+v", gl)
	}
	for _, id := range pending {
		guests.Admit(id)
	}
	if res, _ := park.do("GET", "/api/me", ""); res.StatusCode != 200 {
		t.Fatalf("admitted by admin: %d", res.StatusCode)
	}

	guests.SetInviteOnly(false)
	stranger := newClient(t, ts.URL)
	if res, _ := stranger.do("GET", "/api/me", ""); res.StatusCode != 200 {
		t.Fatalf("open mode: %d", res.StatusCode)
	}
}

func TestBrowse(t *testing.T) {
	ts, f, _ := newTestServer(t)
	f.Library = []source.Track{
		{Source: "fake", ID: "a", Title: "Alpha", Artist: "X", ArtistID: "x", Album: "One", AlbumID: "y", Year: 1999},
		{Source: "fake", ID: "b", Title: "Beta", Artist: "X", ArtistID: "x", Album: "One", AlbumID: "y", Year: 1999},
		{Source: "fake", ID: "c", Title: "Gamma", Artist: "Z", ArtistID: "z"},
	}
	c := newClient(t, ts.URL)
	res, out := c.do("GET", "/api/artist?source=fake&id=x", "")
	if res.StatusCode != 200 || out["name"] != "X" || len(out["tracks"].([]any)) != 2 || len(out["albums"].([]any)) != 1 {
		t.Fatalf("artist: %d %v", res.StatusCode, out)
	}
	res, out = c.do("GET", "/api/artist?source=fake&id=z", "")
	if res.StatusCode != 200 || len(out["albums"].([]any)) != 0 { // no album: [] not null
		t.Fatalf("artist without albums: %d %v", res.StatusCode, out)
	}
	res, out = c.do("GET", "/api/album?source=fake&id=y", "")
	if res.StatusCode != 200 || out["artist"] != "X" || out["year"] != 1999.0 || len(out["tracks"].([]any)) != 2 {
		t.Fatalf("album: %d %v", res.StatusCode, out)
	}
	if res, out = c.do("GET", "/api/album?source=fake&id=nope", ""); res.StatusCode != 404 || out["error"] != "not_found" {
		t.Fatalf("unknown album: %d %v", res.StatusCode, out)
	}
	if res, _ = c.do("GET", "/api/artist?source=fake", ""); res.StatusCode != 404 {
		t.Fatalf("missing id: %d", res.StatusCode)
	}
	if res, out = c.do("GET", "/api/artist?source=other&id=x", ""); res.StatusCode != 502 || out["error"] != "no_source" {
		t.Fatalf("disabled source: %d %v", res.StatusCode, out)
	}

	// folders: top level without an id; empty lists are simply omitted
	f.Folders = map[string]source.Folder{
		"":  {Count: 1, Folders: []source.Folder{{ID: "d", Name: "Music", Count: 1}}},
		"d": {ID: "d", Name: "Music", Count: 1, Path: []source.Folder{{}}, Tracks: f.Library[:1]},
	}
	res, out = c.do("GET", "/api/folder?source=fake", "")
	if res.StatusCode != 200 || len(out["folders"].([]any)) != 1 || out["tracks"] != nil {
		t.Fatalf("top folder: %d %v", res.StatusCode, out)
	}
	res, out = c.do("GET", "/api/folder?source=fake&id=d", "")
	if res.StatusCode != 200 || out["name"] != "Music" || len(out["tracks"].([]any)) != 1 || len(out["path"].([]any)) != 1 {
		t.Fatalf("folder: %d %v", res.StatusCode, out)
	}
	if res, out = c.do("GET", "/api/folder?source=fake&id=nope", ""); res.StatusCode != 404 || out["error"] != "not_found" {
		t.Fatalf("unknown folder: %d %v", res.StatusCode, out)
	}
}
