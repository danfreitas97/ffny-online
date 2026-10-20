package accounts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDiscord answers the token exchange and /users/@me like Discord does.
func fakeDiscord(t *testing.T, user discordUser) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, _ := r.BasicAuth()
		if id != "cid" || secret != "csecret" || r.FormValue("code") != "good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.FormValue("redirect_uri") != "https://play.example/auth/discord/callback" {
			t.Errorf("redirect_uri %q", r.FormValue("redirect_uri"))
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "dtok", "token_type": "Bearer"})
	})
	mux.HandleFunc("GET /users/@me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dtok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(user)
	})
	return httptest.NewServer(mux)
}

func newTestServer(t *testing.T, user discordUser) (*Server, *Store, http.Handler) {
	d := fakeDiscord(t, user)
	t.Cleanup(d.Close)
	store, err := Open(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{PublicURL: "https://play.example", ClientID: "cid", ClientSecret: "csecret",
		DiscordAPI: d.URL, DiscordAuth: "https://discord.example/authorize"}, store)
	return s, store, s.Handler()
}

func do(h http.Handler, method, target, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func signIn(t *testing.T, h http.Handler, secret, code string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, "GET", "/auth/discord/start?login="+hashHex(secret), "", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("start: %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if q.Get("scope") != "identify" || q.Get("client_id") != "cid" || q.Get("state") == "" {
		t.Fatalf("authorize url %s", loc)
	}
	if strings.Contains(loc.String(), secret) {
		t.Fatal("the secret must never reach the browser")
	}
	if r := do(h, "POST", "/auth/poll", `{"secret":"`+secret+`"}`, ""); r.Code != http.StatusAccepted {
		t.Fatalf("poll before the callback: %d", r.Code)
	}
	return do(h, "GET", "/auth/discord/callback?code="+code+"&state="+q.Get("state"), "", "")
}

func TestSignInFlow(t *testing.T) {
	_, store, h := newTestServer(t, discordUser{ID: "42", Username: "masa", GlobalName: "Masa", Avatar: "abc"})
	secret := randomHex(32)
	if rec := signIn(t, h, secret, "good"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "signed in as Masa") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	rec := do(h, "POST", "/auth/poll", `{"secret":"`+secret+`"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("poll: %d", rec.Code)
	}
	var got struct {
		Token   string        `json:"token"`
		Account PublicAccount `json:"account"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Account.Name != "Masa" || got.Account.AvatarURL != "https://cdn.discordapp.com/avatars/42/abc.png?size=128" || !hex64.MatchString(got.Token) {
		t.Fatalf("poll body %+v", got)
	}
	// the finished sign-in is used up
	if r := do(h, "POST", "/auth/poll", `{"secret":"`+secret+`"}`, ""); r.Code != http.StatusNotFound {
		t.Fatalf("second poll: %d", r.Code)
	}
	if r := do(h, "GET", "/api/me", "", got.Token); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"name":"Masa"`) {
		t.Fatalf("me: %d %s", r.Code, r.Body.String())
	}
	if r := do(h, "GET", "/api/me", "", randomHex(32)); r.Code != http.StatusUnauthorized {
		t.Fatalf("me with a wrong token: %d", r.Code)
	}
	// signing in again keeps the account
	secret2 := randomHex(32)
	signIn(t, h, secret2, "good")
	do(h, "POST", "/auth/poll", `{"secret":"`+secret2+`"}`, "")
	if store.Count() != 1 {
		t.Fatalf("accounts: %d", store.Count())
	}
	if r := do(h, "POST", "/auth/logout", "", got.Token); r.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", r.Code)
	}
	if r := do(h, "GET", "/api/me", "", got.Token); r.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", r.Code)
	}
}

func TestCancelledAndBadSignIns(t *testing.T) {
	_, _, h := newTestServer(t, discordUser{ID: "42", Username: "masa"})
	if r := do(h, "GET", "/auth/discord/start?login=nothex", "", ""); r.Code != http.StatusBadRequest {
		t.Fatalf("bad login: %d", r.Code)
	}
	if r := do(h, "GET", "/auth/discord/callback?code=good&state=unknown", "", ""); r.Code != http.StatusBadRequest {
		t.Fatalf("unknown state: %d", r.Code)
	}
	secret := randomHex(32)
	if r := signIn(t, h, secret, "bad"); r.Code != http.StatusBadGateway {
		t.Fatalf("refused code: %d", r.Code)
	}
	if r := do(h, "POST", "/auth/poll", `{"secret":"`+secret+`"}`, ""); r.Code != http.StatusGone {
		t.Fatalf("poll after a failure: %d", r.Code)
	}
	if r := do(h, "POST", "/auth/poll", `{"secret":"x"}`, ""); r.Code != http.StatusBadRequest {
		t.Fatalf("bad secret: %d", r.Code)
	}
}

func TestSignInsRunOut(t *testing.T) {
	s, _, h := newTestServer(t, discordUser{ID: "42", Username: "masa"})
	now := time.Now()
	s.now = func() time.Time { return now }
	secret := randomHex(32)
	do(h, "GET", "/auth/discord/start?login="+hashHex(secret), "", "")
	now = now.Add(LoginLifetime + time.Second)
	if r := do(h, "POST", "/auth/poll", `{"secret":"`+secret+`"}`, ""); r.Code != http.StatusNotFound {
		t.Fatalf("poll after the lifetime: %d", r.Code)
	}
}

func TestDeleteAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	store, _ := Open(path)
	now := time.Now()
	a := store.Upsert("42", "masa", "Masa", "", now)
	b := store.Upsert("43", "dmx", "DMX", "", now)
	tokA, tokB := store.NewSession(a.ID, now), store.NewSession(b.ID, now)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := again.Session(tokA, now); !ok || got.Name != "Masa" {
		t.Fatalf("reloaded session: %v %+v", ok, got)
	}
	if c := again.Upsert("44", "x", "X", "", now); c.ID != 3 {
		t.Fatalf("ids continue after a reload: %d", c.ID)
	}
	s := NewServer(Config{PublicURL: "https://play.example"}, again)
	if r := do(s.Handler(), "DELETE", "/api/me", "", tokA); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", r.Code)
	}
	if _, ok := again.Session(tokA, now); ok {
		t.Fatal("deleted account still signed in")
	}
	if _, ok := again.Session(tokB, now); !ok {
		t.Fatal("other account lost its session")
	}
	if _, ok := again.Session(tokB, now.Add(SessionLifetime+time.Hour)); ok {
		t.Fatal("an unused session must run out")
	}
	if r := do(s.Handler(), "GET", "/privacy", "", ""); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "identify") {
		t.Fatalf("privacy: %d", r.Code)
	}
}
