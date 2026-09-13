package main

import (
	"bufio"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hwhang0917/vibe-music/internal/player"
	"github.com/hwhang0917/vibe-music/internal/server"
	"github.com/hwhang0917/vibe-music/internal/source"
	"github.com/hwhang0917/vibe-music/internal/source/fake"
	"github.com/hwhang0917/vibe-music/internal/store"
)

// A reset must reach connected guests as a history_reset event so they reload.
func TestResetPlayHistoryReachesGuests(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	p := player.New(player.Options{Sources: []source.Source{fake.New(source.Track{ID: "a"})}})
	a := &App{db: db, player: p, guests: server.NewGuests(db, false, func() {})}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if _, err := a.StartServer(port); err != nil {
		t.Fatal(err)
	}
	defer a.StopServer()
	res, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	time.Sleep(100 * time.Millisecond) // let the stream subscribe before the reset
	if err := a.ResetPlayHistory(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case l := <-lines:
			if strings.Contains(l, `"history_reset"`) {
				return
			}
		case <-deadline:
			t.Fatal("no history_reset event on the guest stream")
		}
	}
}
