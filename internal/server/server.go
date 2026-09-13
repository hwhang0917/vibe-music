// Package server is the guest-facing HTTP service: a JSON API under /api, a
// Server-Sent Events stream, and the embedded Vue guest UI for everything else.
// Guests never see a Spotify token; every source call goes through the player.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/hwhang0917/vibe-music/internal/player"
	"github.com/hwhang0917/vibe-music/internal/source"
	"github.com/hwhang0917/vibe-music/internal/store"
)

const (
	guestCookie   = "vm_guest"
	cookieMaxAge  = 365 * 24 * 60 * 60
	maxNameLen    = 24
	searchLimit   = 20
	ssePing       = 30 * time.Second
	requestBodyKB = 4
)

// TopFunc returns the most played tracks of a source (play history).
type TopFunc func(sourceID string, limit int) ([]source.Track, error)

// Version is shown to guests (About dialog); set by the app at startup.
var Version = "dev"

type Server struct {
	player  *player.Player
	guests  *Guests
	top     TopFunc
	onError func(error) // optional: the app shows source failures in the admin window
}

const (
	topLimit   = 10
	chartLimit = 50
)

// Error codes the guest UI translates.
const (
	errNameRequired = "name_required"
	errNameInvalid  = "name_invalid"
	errBlocked      = "blocked"
	errNotInQueue   = "not_in_queue"
	errNotOwner     = "not_owner"
	errInviteNeeded = "invite_required"
	errInviteBad    = "invite_invalid"
	errSearchFailed = "search_failed"
	errNoSource     = "no_source"
	errTrackNeeded  = "track_required"
	errNoPlayer     = "player_not_running"
)

// joinPath is where an invitation link lands: /join?invitationCode=XXXX-XXXX.
const (
	joinPath  = "/join"
	joinParam = "invitationCode"
)

// NewHandler wires the API and serves dist (a built Vite app) as an SPA:
// unknown paths fall back to index.html so client-side routing works.
func NewHandler(dist fs.FS, p *player.Player, guests *Guests, top TopFunc, onError func(error)) http.Handler {
	if guests == nil {
		panic(errNoStore)
	}
	s := &Server{player: p, guests: guests, top: top, onError: onError}
	r := chi.NewRouter()
	r.Use(requestLog, middleware.Recoverer, noRobots, noStore)

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		})
		r.Post("/join", s.join) // before the guest gate: the caller is not admitted yet
		r.Group(func(r chi.Router) {
			r.Use(s.guest)
			r.Get("/state", s.state)
			r.Get("/events", s.events)
			r.Get("/me", s.me)
			r.Post("/me", s.setMe)
			r.Get("/search", s.search)
			r.Get("/top", s.topTracks)
			r.Get("/chart", s.chart)
			r.Get("/artist", s.artist)
			r.Get("/album", s.album)
			r.Get("/folder", s.folder)
			r.Get("/artwork/{source}/{id}", s.artwork)
			r.Group(func(r chi.Router) {
				r.Use(s.requireName)
				r.Post("/queue", s.request)
				r.Post("/queue/{id}/vote", s.vote)
				r.Delete("/queue/{id}", s.remove)
				r.Post("/skip", s.skip)
				r.Delete("/skip", s.unskip)
			})
		})
	})

	r.NotFound(spa(dist))
	return r
}

type ctxKey int

const guestKey ctxKey = 0

// guestID returns the caller's identity: the SHA-256 of their cookie, issuing
// a cookie first if they have none. Only the hash ever leaves this function.
func guestID(w http.ResponseWriter, r *http.Request) string {
	secret := ""
	if c, err := r.Cookie(guestCookie); err == nil && len(c.Value) == 32 {
		secret = c.Value
	}
	if secret == "" {
		secret = newGuestID()
		http.SetCookie(w, &http.Cookie{
			Name: guestCookie, Value: secret, Path: "/", MaxAge: cookieMaxAge,
			HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
	}
	return store.Hash(secret)
}

// guest records the visit, enforces blocks and invite-only, and puts the Guest in ctx.
func (s *Server) guest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := guestID(w, r)
		gu, err := s.guests.seen(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		switch {
		case gu.Blocked:
			writeError(w, http.StatusForbidden, errBlocked)
			return
		case s.guests.InviteOnly() && !gu.Admitted:
			writeError(w, http.StatusForbidden, errInviteNeeded)
			return
		}
		g := player.Guest{ID: id, Name: gu.Name}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), guestKey, g)))
	})
}

// join redeems an invitation code for the caller. The link itself
// (joinPath?joinParam=CODE) is served as the SPA, which then POSTs here: chat
// apps fetch every URL they see for a preview, and a GET that redeemed would
// let the previewer spend the single use before the person does.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	id := guestID(w, r)
	if gu, err := s.guests.db.Seen(id, time.Now()); err != nil || gu.Blocked {
		writeError(w, http.StatusForbidden, errBlocked)
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if err := s.guests.Redeem(in.Code, id); err != nil {
		slog.Warn("guest", "action", "join_refused", "guest", id, "ip", r.RemoteAddr)
		writeError(w, http.StatusForbidden, errInviteBad)
		return
	}
	slog.Info("guest", "action", "join", "guest", id, "ip", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func guestFrom(r *http.Request) player.Guest {
	g, _ := r.Context().Value(guestKey).(player.Guest)
	return g
}

func (s *Server) requireName(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if guestFrom(r).Name == "" {
			writeError(w, http.StatusBadRequest, errNameRequired)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	if s.player == nil {
		writeError(w, http.StatusServiceUnavailable, errNoPlayer)
		return
	}
	writeJSON(w, http.StatusOK, s.stateFor(guestFrom(r)))
}

// stateFor marks which queue items belong to this guest. The queue slice is
// copied so subscribers never share a mutated snapshot.
func (s *Server) stateFor(g player.Guest) player.State {
	return markMine(s.player.State(), g.ID, s.player.HasSkipVote(g.ID))
}

func markMine(st player.State, guestID string, votedSkip bool) player.State {
	st.MySkipVote = votedSkip
	q := make([]player.QueueItem, len(st.Queue))
	for i, it := range st.Queue {
		it.Mine = it.RequestedBy == guestID
		q[i] = it
	}
	st.Queue = q
	return st
}

// events streams player state as SSE `state` events, with a comment ping for keepalive.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if s.player == nil {
		writeError(w, http.StatusServiceUnavailable, errNoPlayer)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	g := guestFrom(r)
	kill, n := s.guests.connect(g.ID)
	s.player.SetConnectedGuests(n)
	defer func() { s.player.SetConnectedGuests(s.guests.disconnect(g.ID, kill)) }()

	ch, cancel := s.player.Subscribe()
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ping := time.NewTicker(ssePing)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-kill:
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case st := <-ch:
			data, _ := json.Marshal(markMine(st, g.ID, s.player.HasSkipVote(g.ID)))
			fmt.Fprintf(w, "event: state\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	g := guestFrom(r)
	writeJSON(w, http.StatusOK, map[string]string{"id": g.ID, "name": g.Name, "version": Version})
}

func (s *Server) setMe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, errNameInvalid)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len([]rune(name)) > maxNameLen {
		writeError(w, http.StatusBadRequest, errNameInvalid)
		return
	}
	g := guestFrom(r)
	if err := s.guests.setName(g.ID, name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	slog.Info("guest", "action", "name", "guest", g.ID, "name", name, "ip", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]string{"id": g.ID, "name": name})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []source.Track{})
		return
	}
	tracks, err := s.player.Search(r.Context(), r.URL.Query().Get("source"), q, searchLimit)
	if err != nil {
		writeError(w, http.StatusBadGateway, s.sourceErr(err))
		return
	}
	if tracks == nil {
		tracks = []source.Track{}
	}
	writeJSON(w, http.StatusOK, tracks)
}

// topTracks lists the most played tracks of one enabled source (?source=;
// defaults to the only enabled one).
func (s *Server) topTracks(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("source")
	if id == "" {
		if ids := s.player.EnabledIDs(); len(ids) == 1 {
			id = ids[0]
		}
	}
	if s.top == nil || !s.player.Enabled(id) {
		writeJSON(w, http.StatusOK, []source.Track{})
		return
	}
	tracks, err := s.top(id, topLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tracks == nil {
		tracks = []source.Track{}
	}
	writeJSON(w, http.StatusOK, tracks)
}

// chart lists a source's Top 50 for a region (?source=&region=).
func (s *Server) chart(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tracks, err := s.player.Chart(r.Context(), q.Get("source"), q.Get("region"), chartLimit)
	if err != nil {
		writeError(w, http.StatusBadGateway, s.sourceErr(err))
		return
	}
	if tracks == nil {
		tracks = []source.Track{}
	}
	writeJSON(w, http.StatusOK, tracks)
}

func (s *Server) request(w http.ResponseWriter, r *http.Request) {
	var t source.Track
	if err := decode(r, &t); err != nil || t.ID == "" {
		writeError(w, http.StatusBadRequest, errTrackNeeded)
		return
	}
	g := guestFrom(r)
	if err := s.player.Request(r.Context(), t, g); err != nil {
		writeError(w, http.StatusConflict, source.ErrorCode(err, errNoSource))
		return
	}
	slog.Info("guest", "action", "request", "guest", g.ID, "name", g.Name, "source", t.Source, "track", t.ID, "title", t.Title)
	writeJSON(w, http.StatusOK, s.stateFor(g))
}

func (s *Server) vote(w http.ResponseWriter, r *http.Request) {
	g := guestFrom(r)
	if err := s.player.Vote(chi.URLParam(r, "id"), g.ID); err != nil {
		writeError(w, http.StatusNotFound, errNotInQueue)
		return
	}
	slog.Info("guest", "action", "vote", "guest", g.ID, "name", g.Name, "item", chi.URLParam(r, "id"))
	writeJSON(w, http.StatusOK, s.stateFor(g))
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	g := guestFrom(r)
	switch err := s.player.Remove(chi.URLParam(r, "id"), g.ID, false); {
	case errors.Is(err, player.ErrNotOwner):
		writeError(w, http.StatusForbidden, errNotOwner)
	case err != nil:
		writeError(w, http.StatusNotFound, errNotInQueue)
	default:
		slog.Info("guest", "action", "remove", "guest", g.ID, "name", g.Name, "item", chi.URLParam(r, "id"))
		writeJSON(w, http.StatusOK, s.stateFor(g))
	}
}

func (s *Server) skip(w http.ResponseWriter, r *http.Request) {
	g := guestFrom(r)
	skipped := s.player.VoteSkip(r.Context(), g.ID)
	slog.Info("guest", "action", "skip_vote", "guest", g.ID, "name", g.Name, "skipped", skipped)
	writeJSON(w, http.StatusOK, map[string]any{"skipped": skipped, "state": s.player.State()})
}

// unskip withdraws the guest's skip vote during the grace period.
func (s *Server) unskip(w http.ResponseWriter, r *http.Request) {
	g := guestFrom(r)
	s.player.UnvoteSkip(g.ID)
	slog.Info("guest", "action", "skip_unvote", "guest", g.ID, "name", g.Name)
	writeJSON(w, http.StatusOK, map[string]any{"state": s.player.State()})
}

func (s *Server) artwork(w http.ResponseWriter, r *http.Request) {
	src, _ := s.player.Source(chi.URLParam(r, "source"))
	ap, ok := src.(source.ArtworkProvider)
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, mime, err := ap.Artwork(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

// --- helpers ---

// sourceErr turns a source failure into what the guest UI shows: a known
// code, or "search_failed: <provider message>" so nothing is hidden. Either
// way the host log gets the full error.
// artist and album serve a source's browse pages (?source=&id=).
func (s *Server) artist(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a, err := s.player.Artist(r.Context(), q.Get("source"), q.Get("id"))
	if q.Get("id") == "" || err != nil {
		s.browseError(w, err)
		return
	}
	if a.Tracks == nil {
		a.Tracks = []source.Track{}
	}
	if a.Albums == nil {
		a.Albums = []source.Album{}
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) album(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a, err := s.player.Album(r.Context(), q.Get("source"), q.Get("id"))
	if q.Get("id") == "" || err != nil {
		s.browseError(w, err)
		return
	}
	if a.Tracks == nil {
		a.Tracks = []source.Track{}
	}
	writeJSON(w, http.StatusOK, a)
}

// folder serves one level of a source's folder tree (?source=&id=; empty id = top).
func (s *Server) folder(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := s.player.Folder(r.Context(), q.Get("source"), q.Get("id"))
	if err != nil {
		s.browseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f) // empty folders/tracks are omitted; the UI defaults them
}

// browseError: a missing page is a plain 404; anything else is the source failing.
func (s *Server) browseError(w http.ResponseWriter, err error) {
	if err == nil || source.ErrorCode(err, "") == source.ErrNotFound.Kind {
		writeError(w, http.StatusNotFound, source.ErrNotFound.Kind)
		return
	}
	writeError(w, http.StatusBadGateway, s.sourceErr(err))
}

func (s *Server) sourceErr(err error) string {
	log.Println("source:", err)
	if s.onError != nil {
		s.onError(err)
	}
	if code := source.ErrorCode(err, ""); code != "" {
		return code
	}
	return errSearchFailed + ": " + err.Error()
}

// noStore keeps phones from running a stale build: the API and the SPA shell
// are never cached. Hashed assets under /assets/ stay cacheable.
// requestLog writes one structured line per request (static assets excluded)
// to the app log for observability; handlers add who-did-what audit lines.
func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			return
		}
		slog.Info("http", "method", r.Method, "path", r.URL.RequestURI(), "status", ww.Status(),
			"bytes", ww.BytesWritten(), "ms", time.Since(start).Milliseconds(), "ip", r.RemoteAddr, "ua", r.UserAgent())
	})
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// noRobots marks every response as not for indexing; this is a private LAN page.
func noRobots(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		next.ServeHTTP(w, r)
	})
}

func spa(dist fs.FS) http.HandlerFunc {
	files := http.FileServer(http.FS(dist))
	return func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(path.Clean(req.URL.Path), "/")
		if name != "" {
			if f, err := dist.Open(name); err == nil {
				f.Close()
				files.ServeHTTP(w, req)
				return
			}
		}
		index, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			http.Error(w, "guest UI not built: run `npm run build` in ./web", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	}
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, requestBodyKB<<10)).Decode(v); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func newGuestID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
