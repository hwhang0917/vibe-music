package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/oauth2"

	"github.com/hwhang0917/vibe-music/internal/config"
	"github.com/hwhang0917/vibe-music/internal/player"
	"github.com/hwhang0917/vibe-music/internal/server"
	"github.com/hwhang0917/vibe-music/internal/source"
	"github.com/hwhang0917/vibe-music/internal/source/local"
	"github.com/hwhang0917/vibe-music/internal/source/spotify"
	"github.com/hwhang0917/vibe-music/internal/source/youtube"
	"github.com/hwhang0917/vibe-music/internal/store"
	"github.com/hwhang0917/vibe-music/web"
)

// shutdownTimeout bounds how long StopServer waits for in-flight requests.
const shutdownTimeout = 5 * time.Second
const deactivateTimeout = 2 * time.Second // per-source cleanup budget on window close

// Wails event names the admin UI listens on.
const (
	stateEvent       = "state"
	guestsEvent      = "guests"
	youtubeEvent     = "yt:cmd"       // commands for the embedded YouTube player
	noticeEvent      = "notice"       // one-off warnings for the admin (toast), by code
	sourceErrorEvent = "source-error" // a guest-facing source failure, verbatim
	localScanEvent   = "local:scan"   // {done, total} while local files are indexed
)

// Sources that need the internet. Offline, startup leaves them off.
var onlineSources = map[string]bool{"spotify": true, "youtube": true}

// netProbeTimeout bounds the connectivity check at startup.
const netProbeTimeout = 3 * time.Second

// online reports whether either provider host answers a TCP dial. Corporate
// networks may block one; both blocked is treated as offline.
// ponytail: a dial, not an HTTP call; good enough to decide what to enable.
func online() bool {
	hosts := []string{"accounts.spotify.com:443", "www.googleapis.com:443"}
	ok := make(chan bool, len(hosts))
	for _, h := range hosts {
		go func(h string) {
			c, err := net.DialTimeout("tcp", h, netProbeTimeout)
			if err == nil {
				c.Close()
			}
			ok <- err == nil
		}(h)
	}
	for range hosts {
		if <-ok {
			return true
		}
	}
	return false
}

// UI error codes. The admin UI translates "code: detail"; unknown text is
// shown as-is. Keep in sync with frontend/src/i18n.ts err.* keys.
const (
	errPortInUse     = "port_in_use"
	errPortDenied    = "port_denied"
	errServerRunning = "server_running"
	errUnknownSource = "unknown_source"
	errAudioOutput   = "audio_output"
)

// Winsock reports its own errno values; Go's syscall.EADDRINUSE/EACCES are
// the POSIX ones and never match on Windows.
const (
	wsaEADDRINUSE syscall.Errno = 10048
	wsaEACCES     syscall.Errno = 10013
)

func isErrno(err error, targets ...syscall.Errno) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	for _, t := range targets {
		if errno == t {
			return true
		}
	}
	return false
}

// codeErr formats an error as "code: detail" so the UI can translate it.
func codeErr(code, detail string) error {
	if detail == "" {
		return errors.New(code)
	}
	return fmt.Errorf("%s: %s", code, detail)
}

// uiError maps well-known failures to codes; anything else passes through.
func uiError(err error) error {
	switch {
	case err == nil:
		return nil
	case isErrno(err, syscall.EADDRINUSE, wsaEADDRINUSE):
		return codeErr(errPortInUse, "")
	case isErrno(err, syscall.EACCES, wsaEACCES):
		return codeErr(errPortDenied, "")
	case errors.Is(err, local.ErrAudioOutput):
		return codeErr(errAudioOutput, err.Error())
	}
	if code := source.ErrorCode(err, ""); code != "" {
		return codeErr(code, "")
	}
	return err
}

// App is the Wails-bound backend for the admin window.
type App struct {
	ctx context.Context

	mu      sync.Mutex
	cfg     config.Config
	db      *store.Store
	srv     *http.Server
	url     string
	ytURL   string // loopback page framing the YouTube player (see youtube.ServePlayer)
	player  *player.Player
	guests  *server.Guests
	local   *local.Source
	spotify *spotify.Source
	youtube *youtube.Source
	cancel  context.CancelFunc
	info    AppInfo
}

// AppInfo is what the admin's Info tab shows.
type AppInfo struct {
	Version string `json:"version"`
	DataDir string `json:"dataDir"`
	DBPath  string `json:"dbPath"`
	LogPath string `json:"logPath"`
}

type ServerStatus struct {
	Running bool   `json:"running"`
	Port    int    `json:"port"`
	URL     string `json:"url"`
}

// SourceStatus is one row in the admin's source picker.
type SourceStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`   // switched on and activated
	Wanted    bool   `json:"wanted"`    // switched on in config (may have failed to activate)
	Exclusive bool   `json:"exclusive"` // Spotify: cannot run beside other sources
	Ready     bool   `json:"ready"`     // configured enough to be switched on
	Detail    string `json:"detail"`
}

func NewApp() *App { return &App{} }

// localCache adapts the store to local.Cache.
type localCache struct{ db *store.Store }

func (c localCache) Get(path string, size int64, mtime time.Time) ([]byte, bool) {
	return c.db.LocalIndexGet(path, size, mtime)
}
func (c localCache) Put(path string, size int64, mtime time.Time, data []byte) {
	c.db.LocalIndexPut(path, size, mtime, data)
}
func (c localCache) Prune(before time.Time) { c.db.LocalIndexPrune(before) }

// audit logs an admin action; guest actions are logged by the server package.
func (a *App) audit(action string, kv ...any) {
	slog.Info("admin", append([]any{"action", action}, kv...)...)
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	dir, err := config.Dir()
	if err != nil {
		log.Fatal(err)
	}
	a.info = AppInfo{Version: version, DataDir: dir, DBPath: filepath.Join(dir, config.DBFile), LogPath: filepath.Join(dir, logFile)}
	if err := setupLog(a.info.LogPath); err != nil {
		log.Println("log file:", err)
	}
	slog.Info("startup", "version", version, "dataDir", dir)
	server.Version = version
	a.db, err = store.Open(a.info.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := config.Load(a.db)
	if err != nil {
		log.Println("config:", err)
		cfg = config.Default()
	}
	a.cfg = cfg
	token, err := config.LoadToken()
	if err != nil {
		log.Println("spotify token:", err)
	}

	a.local = local.New(cfg.Local.Folders)
	a.local.SetCache(localCache{a.db})
	a.local.SetProgress(func(done, total int) {
		runtime.EventsEmit(a.ctx, localScanEvent, map[string]int{"done": done, "total": total})
	})
	a.spotify = spotify.New(spotify.Options{
		ClientID:     cfg.Spotify.ClientID,
		DeviceID:     cfg.Spotify.DeviceID,
		CallbackPort: cfg.Spotify.CallbackPort,
		Token:        token,
		SaveToken:    a.saveSpotifyToken,
		OpenBrowser:  func(u string) error { runtime.BrowserOpenURL(ctx, u); return nil },
	})
	ytKey, err := config.LoadYouTubeKey()
	if err != nil {
		log.Println("youtube key:", err)
	}
	a.youtube = youtube.New(youtube.Options{
		APIKey: ytKey,
		Send:   func(c youtube.Command) { runtime.EventsEmit(a.ctx, youtubeEvent, c) },
	})
	if a.ytURL, err = youtube.ServePlayer(); err != nil {
		log.Println("youtube player:", err)
	}
	a.player = player.New(player.Options{
		Sources:   []source.Source{a.local, a.spotify, a.youtube},
		SkipRatio: cfg.SkipRatio,
	})
	a.player.SetPersister(func(items []player.PersistedItem) {
		if err := a.db.SaveQueue(toRows(items)); err != nil {
			log.Println("save queue:", err)
		}
	})
	a.player.SetOnPlay(func(sourceID string, t source.Track) {
		track, _ := json.Marshal(t)
		if err := a.db.RecordPlay(sourceID, t.ID, track, time.Now()); err != nil {
			log.Println("record play:", err)
		}
	})
	if rows, err := a.db.LoadQueue(); err == nil {
		a.player.Restore(fromRows(rows))
	} else {
		log.Println("load queue:", err)
	}
	a.guests = server.NewGuests(a.db, cfg.InviteOnly, func() {
		if list, err := a.guests.List(); err == nil {
			runtime.EventsEmit(a.ctx, guestsEvent, list)
		}
	})

	runCtx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go a.player.Run(runCtx)
	go a.forwardState(runCtx)

	// Enabling can touch the network (Spotify verifies devices). Never do that
	// before the window is up: Wails shows it only after startup returns.
	go a.enableConfigured(runCtx, cfg.Enabled)
}

// enableConfigured switches on the sources saved in config. Offline, the
// online-only ones stay off (still wanted in config) and the admin is warned.
func (a *App) enableConfigured(ctx context.Context, ids []string) {
	isOnline := true
	for _, id := range ids {
		if onlineSources[id] {
			isOnline = online()
			break
		}
	}
	skipped := false
	for _, id := range ids {
		if onlineSources[id] && !isOnline {
			skipped = true
			continue
		}
		if err := a.player.SetEnabled(ctx, id, true); err != nil {
			log.Println("enable", id+":", err) // stays wanted in config; shows as not started
		}
	}
	if skipped {
		runtime.EventsEmit(a.ctx, noticeEvent, "offline_sources_off")
	}
}

func (a *App) shutdown(ctx context.Context) {
	_ = a.StopServer()
	if a.cancel != nil {
		a.cancel()
	}
	// Deactivate may call out (Spotify pause); the window stays open until this
	// returns, so bound it rather than wait on a slow network.
	ctx, cancel := context.WithTimeout(ctx, deactivateTimeout)
	defer cancel()
	for _, id := range a.player.EnabledIDs() {
		if src, ok := a.player.Source(id); ok {
			_ = src.Deactivate(ctx)
		}
	}
	if a.db != nil {
		_ = a.db.Close()
	}
}

// topTracks adapts the store's play history to the server's TopFunc.
func (a *App) topTracks(sourceID string, limit int) ([]source.Track, error) {
	raws, err := a.db.TopTracks(sourceID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]source.Track, 0, len(raws))
	for _, r := range raws {
		var t source.Track
		if json.Unmarshal(r, &t) == nil {
			out = append(out, t)
		}
	}
	return out, nil
}

func toRows(items []player.PersistedItem) []store.QueueRow {
	out := make([]store.QueueRow, 0, len(items))
	for _, it := range items {
		track, _ := json.Marshal(it.Track)
		out = append(out, store.QueueRow{
			ID: it.ID, Track: track, RequestedBy: it.RequestedBy, RequestedByName: it.RequestedByName,
			RequestedAt: it.RequestedAt, Rank: it.Rank, Votes: it.Votes,
		})
	}
	return out
}

func fromRows(rows []store.QueueRow) []player.PersistedItem {
	out := make([]player.PersistedItem, 0, len(rows))
	for _, r := range rows {
		var track source.Track
		if err := json.Unmarshal(r.Track, &track); err != nil {
			continue // a row we cannot read is not worth crashing over
		}
		out = append(out, player.PersistedItem{
			ID: r.ID, Track: track, RequestedBy: r.RequestedBy, RequestedByName: r.RequestedByName,
			RequestedAt: r.RequestedAt, Rank: r.Rank, Votes: r.Votes,
		})
	}
	return out
}

// forwardState pushes player snapshots to the admin window as Wails events.
func (a *App) forwardState(ctx context.Context) {
	ch, cancel := a.player.Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case st := <-ch:
			runtime.EventsEmit(a.ctx, stateEvent, st)
		}
	}
}

func (a *App) saveSpotifyToken(tok *oauth2.Token) {
	if err := config.SaveToken(tok); err != nil {
		log.Println("save token:", err)
	}
}

// saveConfig persists the in-memory config.
func (a *App) saveConfig() error {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	return config.Save(a.db, cfg)
}

// --- config ---

func (a *App) GetConfig() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.cfg
	c.YouTube.HasKey = a.youtube.HasAPIKey()
	return c
}

// SaveConfig persists and applies settings. Changing the Spotify Client ID
// requires SpotifyConnect afterwards. InviteOnly is toggled via SetInviteOnly.
func (a *App) SaveConfig(c config.Config) error {
	a.audit("save_config", "port", c.Port, "skipRatio", c.SkipRatio, "inviteOnly", c.InviteOnly, "enabled", c.Enabled, "folders", len(c.Local.Folders))
	if c.Local.Folders == nil {
		c.Local.Folders = []string{}
	}
	if c.Spotify.CallbackPort <= 0 {
		c.Spotify.CallbackPort = config.DefaultSpotifyCallbackPort
	}
	a.mu.Lock()
	c.InviteOnly = a.cfg.InviteOnly
	a.cfg = c
	a.mu.Unlock()
	if err := config.Save(a.db, c); err != nil {
		return err
	}
	a.local.SetFolders(c.Local.Folders)
	a.spotify.SetClientID(c.Spotify.ClientID)
	a.spotify.SetDeviceID(c.Spotify.DeviceID)
	a.spotify.SetCallbackPort(c.Spotify.CallbackPort)
	a.player.SetSkipRatio(c.SkipRatio)
	return nil
}

// --- sources ---

func (a *App) Sources() []SourceStatus {
	cfg := a.GetConfig()
	out := make([]SourceStatus, 0, 3)
	for _, info := range a.player.Sources() {
		st := SourceStatus{ID: info.ID, Name: info.Name, Enabled: info.Enabled, Wanted: cfg.IsEnabled(info.ID), Exclusive: info.Exclusive}
		switch info.ID {
		case a.local.ID():
			st.Ready = len(cfg.Local.Folders) > 0
			st.Detail = fmt.Sprintf("%d folder(s)", len(cfg.Local.Folders))
		case a.spotify.ID():
			st.Ready = a.spotify.Connected()
			if st.Ready {
				st.Detail = "connected"
			} else {
				st.Detail = "not connected"
			}
		case a.youtube.ID():
			st.Ready = a.youtube.HasAPIKey()
			if st.Ready {
				st.Detail = "API key set"
			} else {
				st.Detail = "API key needed"
			}
		}
		out = append(out, st)
	}
	return out
}

// SetSourceEnabled switches a source on or off and remembers it. Turning on an
// exclusive source (Spotify) turns every other source off first, and turning
// on any source while Spotify is on turns Spotify off; the UI confirms both.
// Turning a source off stops it if playing and drops its queued items.
func (a *App) SetSourceEnabled(id string, enabled bool) error {
	a.audit("set_source", "source", id, "enabled", enabled)
	if _, ok := a.sourceByID(id); !ok {
		return codeErr(errUnknownSource, id)
	}
	if enabled {
		for _, other := range a.player.EnabledIDs() {
			if other != id && (player.ExclusiveSources[id] || player.ExclusiveSources[other]) {
				if err := a.SetSourceEnabled(other, false); err != nil {
					return err
				}
			}
		}
	}
	if err := a.player.SetEnabled(a.ctx, id, enabled); err != nil {
		return uiError(err)
	}
	a.mu.Lock()
	a.cfg.SetEnabled(id, enabled)
	a.mu.Unlock()
	return a.saveConfig()
}

func (a *App) sourceByID(id string) (player.SourceInfo, bool) {
	for _, s := range a.player.Sources() {
		if s.ID == id {
			return s, true
		}
	}
	return player.SourceInfo{}, false
}

func (a *App) LocalRescan() (int, error) { a.audit("local_rescan"); return a.local.Rescan() }

// PickFolder opens the OS directory chooser. Returns "" when cancelled.
func (a *App) PickFolder() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: "Add music folder"})
}

// SpotifyConnect runs the browser consent flow; blocks until finished.
func (a *App) SpotifyConnect() error {
	a.audit("spotify_connect")
	return uiError(a.spotify.Connect(a.ctx))
}

func (a *App) SpotifyDisconnect() { a.audit("spotify_disconnect"); a.spotify.Disconnect() }

// SpotifyRedirectURI is the exact URI to register in the Spotify dashboard.
func (a *App) SpotifyRedirectURI() string { return a.spotify.RedirectURI() }

// SpotifyCancelConnect aborts a pending Connect (the admin closed the browser tab).
func (a *App) SpotifyCancelConnect() { a.spotify.CancelConnect() }

// SpotifyReset forgets everything about Spotify: switches it off (dropping
// its queued songs), deletes the saved token, and clears the stored Client ID
// and device. One call so nothing is left half-done.
func (a *App) SpotifyReset() error {
	a.audit("spotify_reset")
	if a.player.Enabled(a.spotify.ID()) {
		if err := a.SetSourceEnabled(a.spotify.ID(), false); err != nil {
			return err
		}
	}
	a.spotify.CancelConnect()
	a.spotify.Disconnect() // removes the token file via SaveToken(nil)
	a.spotify.SetClientID("")
	a.spotify.SetDeviceID("")
	a.mu.Lock()
	a.cfg.Spotify.ClientID = ""
	a.cfg.Spotify.DeviceID = ""
	a.mu.Unlock()
	return a.saveConfig()
}

func (a *App) SpotifyDevices() ([]spotify.Device, error) {
	d, err := a.spotify.Devices(a.ctx)
	return d, uiError(err)
}

// --- guests & invitations ---

func (a *App) Guests() ([]server.GuestInfo, error) { return a.guests.List() }

// SetInviteOnly toggles invitation-only access. Turning it on keeps everyone
// already known admitted; newcomers need a code.
func (a *App) SetInviteOnly(on bool) error {
	a.audit("invite_only", "on", on)
	if err := a.guests.SetInviteOnly(on); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg.InviteOnly = on
	a.mu.Unlock()
	return a.saveConfig()
}

// AdmitGuest lets a specific guest in without a code.
func (a *App) AdmitGuest(id string) error { a.audit("admit", "guest", id); return a.guests.Admit(id) }

// CreateInvitation makes a code valid for ttlMinutes; the plaintext is only
// available in the returned value and in this session's list.
func (a *App) CreateInvitation(ttlMinutes int) (server.Invitation, error) {
	a.audit("invitation_create", "ttlMinutes", ttlMinutes)
	if ttlMinutes <= 0 {
		return server.Invitation{}, errors.New("ttl must be positive")
	}
	return a.guests.CreateInvitation(time.Duration(ttlMinutes) * time.Minute)
}

func (a *App) Invitations() ([]server.Invitation, error) { return a.guests.Invitations() }

func (a *App) RevokeInvitation(id int64) error {
	a.audit("invitation_revoke", "id", id)
	return a.guests.RevokeInvitation(id)
}

// KickGuest drops the guest's live connections; they can reconnect.
func (a *App) KickGuest(id string) { a.audit("kick", "guest", id); a.guests.Kick(id) }

// RemoveGuest forgets the guest and everything they queued or voted for.
func (a *App) RemoveGuest(id string) error {
	a.audit("remove_guest", "guest", id)
	a.player.RemoveGuest(id)
	return a.guests.Remove(id)
}

// BlockGuest toggles the (persisted) block. Blocking also removes the guest's
// requests and votes.
func (a *App) BlockGuest(id string, blocked bool) error {
	a.audit("block", "guest", id, "blocked", blocked)
	if blocked {
		a.player.RemoveGuest(id)
	}
	return a.guests.SetBlocked(id, blocked)
}

// --- queue (admin override) ---

// RemoveQueueItem drops any queued item regardless of who requested it.
func (a *App) RemoveQueueItem(id string) error {
	a.audit("queue_remove", "item", id)
	return a.player.Remove(id, "", true)
}

// MoveQueueItem places a queued item at index (0 = next up).
func (a *App) MoveQueueItem(id string, index int) error {
	a.audit("queue_move", "item", id, "index", index)
	return a.player.Move(id, index)
}

// Seek moves the current track to the given millisecond offset.
func (a *App) Seek(ms int) error {
	a.audit("seek", "ms", ms)
	return uiError(a.player.Seek(a.ctx, time.Duration(ms)*time.Millisecond))
}

// --- youtube ---

// SetYouTubeAPIKey stores the Data API key (empty removes it).
func (a *App) SetYouTubeAPIKey(key string) error {
	a.audit("youtube_key", "set", strings.TrimSpace(key) != "") // never the key itself
	if err := youtube.ValidateAPIKey(key); err != nil {
		return uiError(err)
	}
	if err := config.SaveYouTubeKey(key); err != nil {
		return err
	}
	a.youtube.SetAPIKey(key)
	return nil
}

// YouTubeTest checks the saved key with a real search and chart call.
func (a *App) YouTubeTest(region string) (string, error) {
	// Deliberately not uiError: the admin wants the exact response here.
	return a.youtube.Test(a.ctx, region)
}

// YouTubeReport receives the embedded player's state from the admin page.
func (a *App) YouTubeReport(r youtube.Report) { a.youtube.Report(r) }

// Artwork returns a track's cover as a data URL for the admin window. Guests
// fetch the same bytes from /api/artwork, but that server may not be running
// while the host is already playing.
func (a *App) Artwork(sourceID, trackID string) (string, error) {
	src, ok := a.player.Source(sourceID)
	ap, isAP := src.(source.ArtworkProvider)
	if !ok || !isAP {
		return "", nil
	}
	data, mime, err := ap.Artwork(a.ctx, trackID)
	if err != nil {
		return "", err
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// YouTubePlayerURL is the loopback page the admin window frames for playback.
func (a *App) YouTubePlayerURL() string { return a.ytURL }

// ResetPlayHistory clears the "most played" lists for every source.
func (a *App) ResetPlayHistory() error {
	a.audit("reset_play_history")
	if err := a.db.ClearPlays(); err != nil {
		return err
	}
	a.player.Announce(player.Event{Type: "history_reset"}) // guests reload their most-played list
	return nil
}

// --- playback (admin override) ---

func (a *App) GetState() player.State  { return a.player.State() }
func (a *App) Pause() error            { a.audit("pause"); return a.player.Pause(a.ctx) }
func (a *App) Resume() error           { a.audit("resume"); return a.player.Resume(a.ctx) }
func (a *App) Skip()                   { a.audit("skip"); a.player.Skip(a.ctx) }
func (a *App) SetVolume(pct int) error { return a.player.SetVolume(a.ctx, pct) }

// --- guest server ---

// StartServer starts the guest HTTP server and returns the LAN URL to share.
func (a *App) StartServer(port int) (string, error) {
	a.audit("server_start", "port", port)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.srv != nil {
		return a.url, codeErr(errServerRunning, "")
	}
	if port <= 0 {
		port = config.DefaultPort
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return "", uiError(err)
	}
	// Shutdown only waits for handlers; the SSE streams never finish on their
	// own, so cancel their base context to end them instead of waiting out
	// shutdownTimeout on every stop.
	srvCtx, cancelSrv := context.WithCancel(context.Background())
	srv := &http.Server{
		Handler: server.NewHandler(web.Dist, a.player, a.guests, a.topTracks, func(err error) {
			runtime.EventsEmit(a.ctx, sourceErrorEvent, err.Error())
		}),
		BaseContext: func(net.Listener) context.Context { return srvCtx },
	}
	srv.RegisterOnShutdown(cancelSrv)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Println("guest server:", err)
		}
	}()
	a.srv = srv
	a.url = fmt.Sprintf("http://%s:%d", lanIP(), port)
	a.cfg.Port = port
	if err := config.Save(a.db, a.cfg); err != nil {
		log.Println("save config:", err)
	}
	return a.url, nil
}

// StopServer gracefully shuts the guest server down. No-op if not running.
func (a *App) StopServer() error {
	a.audit("server_stop")
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := a.srv.Shutdown(ctx)
	a.srv, a.url = nil, ""
	return err
}

// Info reports where this install keeps its data and logs.
func (a *App) Info() AppInfo { return a.info }

func (a *App) Status() ServerStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return ServerStatus{Running: a.srv != nil, Port: a.cfg.Port, URL: a.url}
}

// lanIP returns the first private IPv4 on a non-loopback interface, falling
// back to loopback so the URL is still openable on the host.
func lanIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil {
			continue
		}
		if ipnet.IP.IsPrivate() {
			return ipnet.IP.String()
		}
	}
	return "127.0.0.1"
}
