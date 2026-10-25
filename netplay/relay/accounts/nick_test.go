package accounts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeGuild answers GET /guilds/g1/members/{id} as the bot sees it: nicks[id] (nil: a member with no nickname;
// missing: not a member).
type fakeGuild struct {
	mu    sync.Mutex
	nicks map[string]*string
	calls int
}

func (f *fakeGuild) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /guilds/g1/members/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot btok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		nick, ok := f.nicks[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"nick": nick, "user": map[string]string{"id": r.PathValue("id")}})
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func str(s string) *string { return &s }

func newNickServer(t *testing.T, g *fakeGuild) (*Server, *Store) {
	store, err := Open(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{PublicURL: "https://play.example", DiscordAPI: g.server(t).URL, BotToken: "btok", GuildID: "g1"}, store)
	return s, store
}

func TestServerNicknameIsTheShownName(t *testing.T) {
	g := &fakeGuild{nicks: map[string]*string{"100": str("King Ann")}}
	s, store := newNickServer(t, g)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	a := store.Upsert("100", "ann_x", "Ann", "", now)

	if a = s.refreshNick(context.Background(), a, true); a.Name != "King Ann" {
		t.Fatalf("with a nickname: %q", a.Name)
	}
	// a fresh lookup is not repeated within NickRefresh (the caller's copy, from its session, comes back)
	g.mu.Lock()
	*g.nicks["100"] = "Queen Ann"
	g.mu.Unlock()
	if got := s.refreshNick(context.Background(), a, false); got.Name != "King Ann" || g.calls != 1 {
		t.Fatalf("within the refresh time: %q after %d calls", got.Name, g.calls)
	}
	now = now.Add(NickRefresh + time.Second)
	if got := s.refreshNick(context.Background(), a, false); got.Name != "Queen Ann" {
		t.Fatalf("renamed: %q", got.Name)
	}
	// the nickname taken off: the Discord display name again
	g.mu.Lock()
	g.nicks["100"] = nil
	g.mu.Unlock()
	if got := s.refreshNick(context.Background(), a, true); got.Name != "Ann" {
		t.Fatalf("no nickname: %q", got.Name)
	}
	// left the server: the display name; a sign-in again keeps the nickname rule
	delete(g.nicks, "100")
	if got := s.refreshNick(context.Background(), a, true); got.Name != "Ann" {
		t.Fatalf("not a member: %q", got.Name)
	}
	g.nicks["100"] = str("Ann the Great")
	s.refreshNick(context.Background(), a, true)
	if got := store.Upsert("100", "ann_x", "Ann", "", now); got.Name != "Ann the Great" {
		t.Fatalf("signed in again: %q", got.Name)
	}
}

func TestNicknameMigratesOldAccounts(t *testing.T) {
	g := &fakeGuild{nicks: map[string]*string{"7": nil}}
	s, store := newNickServer(t, g)
	a := store.Upsert("7", "old_user", "Old Display", "", time.Now())
	// an account saved before display names were kept apart: Name only
	store.mu.Lock()
	store.accounts[a.ID].Global = ""
	store.mu.Unlock()
	g.nicks["7"] = str("Nick")
	if got := s.refreshNick(context.Background(), a, true); got.Name != "Nick" {
		t.Fatalf("nickname: %q", got.Name)
	}
	g.nicks["7"] = nil
	if got := s.refreshNick(context.Background(), a, true); got.Name != "Old Display" {
		t.Fatalf("back to the old display name: %q", got.Name)
	}
}

func TestNicknamesOffWithoutTheBot(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{PublicURL: "https://play.example", DiscordAPI: "http://127.0.0.1:9"}, store)
	a := store.Upsert("1", "u", "Display", "", time.Now())
	if got := s.refreshNick(context.Background(), a, true); got.Name != "Display" {
		t.Fatalf("%q", got.Name)
	}
}
