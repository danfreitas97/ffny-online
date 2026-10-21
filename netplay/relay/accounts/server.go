package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// How the launcher signs in, without ever holding the Discord client secret:
//
//  1. It makes a random secret and opens the browser on /auth/discord/start?login=SHA256(secret).
//  2. The service sends the browser to Discord (scope identify) and, on the way back to
//     /auth/discord/callback, exchanges the code, reads the user and makes or updates the account.
//  3. The launcher polls /auth/poll with the secret itself; only it can turn the finished login
//     into a session token. The browser and its history only ever see the hash.

// Config is the service's setup.
type Config struct {
	PublicURL    string // e.g. https://play.defjam.online
	ClientID     string
	ClientSecret string
	DiscordAPI   string // https://discord.com/api (tests point it elsewhere)
	DiscordAuth  string // https://discord.com/oauth2/authorize
}

// LoginLifetime is how long a sign-in may take in the browser.
const LoginLifetime = 10 * time.Minute

const maxPending = 5000

type pending struct {
	created time.Time
	state   string
	account int64 // set when Discord sent the player back
	failed  string
}

// Server is the account service's HTTP side.
type Server struct {
	cfg     Config
	store   *Store
	client  *http.Client
	now     func() time.Time
	mu      sync.Mutex
	logins  map[string]*pending // key: login (SHA-256 of the launcher's secret)
	byState map[string]string   // OAuth state -> login
}

// NewServer makes the service for a store.
func NewServer(cfg Config, store *Store) *Server {
	if cfg.DiscordAPI == "" {
		cfg.DiscordAPI = "https://discord.com/api"
	}
	if cfg.DiscordAuth == "" {
		cfg.DiscordAuth = "https://discord.com/oauth2/authorize"
	}
	return &Server{cfg: cfg, store: store, client: &http.Client{Timeout: 15 * time.Second}, now: time.Now,
		logins: map[string]*pending{}, byState: map[string]string{}}
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Handler is the service's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/discord/start", s.start)
	mux.HandleFunc("GET /auth/discord/callback", s.callback)
	mux.HandleFunc("POST /auth/poll", s.poll)
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("DELETE /api/me", s.deleteMe)
	mux.HandleFunc("POST /api/matches", s.report)
	mux.HandleFunc("GET /api/me/stats", s.myStats)
	mux.HandleFunc("GET /api/leaderboard", s.leaderboard)
	mux.HandleFunc("POST /api/ranked/ticket", s.rankedTicket)
	mux.HandleFunc("GET /privacy", s.privacy)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}

func (s *Server) redirectURI() string { return s.cfg.PublicURL + "/auth/discord/callback" }

// expire drops sign-ins older than LoginLifetime. Callers hold s.mu.
func (s *Server) expire() {
	now := s.now()
	for k, p := range s.logins {
		if now.Sub(p.created) > LoginLifetime {
			delete(s.byState, p.state)
			delete(s.logins, k)
		}
	}
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	login := r.URL.Query().Get("login")
	if !hex64.MatchString(login) {
		page(w, http.StatusBadRequest, "Sign-in link not valid", "Start the sign-in again from the launcher.")
		return
	}
	s.mu.Lock()
	s.expire()
	p, ok := s.logins[login]
	if !ok {
		if len(s.logins) >= maxPending {
			s.mu.Unlock()
			page(w, http.StatusServiceUnavailable, "Too many sign-ins right now", "Try again in a few minutes.")
			return
		}
		p = &pending{created: s.now(), state: randomHex(16)}
		s.logins[login] = p
		s.byState[p.state] = login
	}
	state := p.state
	s.mu.Unlock()
	q := url.Values{"response_type": {"code"}, "client_id": {s.cfg.ClientID}, "scope": {"identify"},
		"state": {state}, "redirect_uri": {s.redirectURI()}, "prompt": {"none"}}
	http.Redirect(w, r, s.cfg.DiscordAuth+"?"+q.Encode(), http.StatusFound)
}

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Avatar     string `json:"avatar"`
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	s.expire()
	login, ok := s.byState[q.Get("state")]
	var p *pending
	if ok {
		p = s.logins[login]
	}
	s.mu.Unlock()
	if p == nil {
		page(w, http.StatusBadRequest, "This sign-in has run out", "Start it again from the launcher.")
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		s.fail(p, "cancelled in Discord")
		page(w, http.StatusOK, "Sign-in cancelled", "Nothing was saved. You can close this window.")
		return
	}
	user, err := s.discordUser(r.Context(), q.Get("code"))
	if err != nil {
		log.Printf("accounts: discord sign-in: %v", err)
		s.fail(p, "Discord did not answer")
		page(w, http.StatusBadGateway, "Discord did not answer", "Try again from the launcher in a moment.")
		return
	}
	name := user.GlobalName
	if name == "" {
		name = user.Username
	}
	a := s.store.Upsert(user.ID, user.Username, name, user.Avatar, s.now())
	s.mu.Lock()
	p.account = a.ID
	s.mu.Unlock()
	log.Printf("accounts: account %d signed in", a.ID)
	page(w, http.StatusOK, "You're signed in as "+a.Name, "Go back to the FFNY Online launcher. You can close this window.")
}

func (s *Server) fail(p *pending, why string) {
	s.mu.Lock()
	p.failed = why
	s.mu.Unlock()
}

// discordUser exchanges the code and reads who signed in. The Discord token is not kept.
func (s *Server) discordUser(ctx context.Context, code string) (discordUser, error) {
	var u discordUser
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.redirectURI()}}
	req, _ := http.NewRequestWithContext(ctx, "POST", s.cfg.DiscordAPI+"/oauth2/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(s.cfg.ClientID, s.cfg.ClientSecret)
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := s.doJSON(req, &tok); err != nil {
		return u, fmt.Errorf("token: %w", err)
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", s.cfg.DiscordAPI+"/users/@me", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if err := s.doJSON(req, &u); err != nil {
		return u, fmt.Errorf("user: %w", err)
	}
	if u.ID == "" {
		return u, fmt.Errorf("user: no id")
	}
	return u, nil
}

func (s *Server) doJSON(req *http.Request, out any) error {
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// PublicAccount is what the launcher and the game get.
type PublicAccount struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
}

func public(a Account) PublicAccount {
	avatar := "https://cdn.discordapp.com/embed/avatars/0.png"
	if a.Avatar != "" {
		avatar = "https://cdn.discordapp.com/avatars/" + a.DiscordID + "/" + a.Avatar + ".png?size=128"
	}
	return PublicAccount{ID: a.ID, Name: a.Name, Username: a.Username, AvatarURL: avatar}
}

func (s *Server) poll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || !hex64.MatchString(body.Secret) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	login := hashHex(body.Secret)
	s.mu.Lock()
	s.expire()
	p, ok := s.logins[login]
	if !ok {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such sign-in"})
		return
	}
	if p.failed != "" {
		why := p.failed
		delete(s.byState, p.state)
		delete(s.logins, login)
		s.mu.Unlock()
		writeJSON(w, http.StatusGone, map[string]string{"error": why})
		return
	}
	if p.account == 0 {
		s.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "waiting"})
		return
	}
	id := p.account
	delete(s.byState, p.state)
	delete(s.logins, login)
	s.mu.Unlock()
	token := s.store.NewSession(id, s.now())
	a, _ := s.store.Session(token, s.now())
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "account": public(a)})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok && hex64.MatchString(t) {
		return t
	}
	return ""
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	a, ok := s.store.Session(bearer(r), s.now())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	writeJSON(w, http.StatusOK, public(a))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.store.EndSession(bearer(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteMe(w http.ResponseWriter, r *http.Request) {
	a, ok := s.store.Session(bearer(r), s.now())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	s.store.Delete(a.ID)
	log.Printf("accounts: account %d deleted by its owner", a.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	a, ok := s.store.Session(bearer(r), s.now())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	var rep MatchReport
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&rep); err != nil || !s.store.Report(a.ID, rep, s.now()) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "report not taken"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "counted once the other players' games agree"})
}

func (s *Server) myStats(w http.ResponseWriter, r *http.Request) {
	a, ok := s.store.Session(bearer(r), s.now())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	st, _ := s.store.StatsOf(a.ID)
	if r.URL.Query().Get("format") == "text" { // for the game: no JSON parser in C
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, StatsText(st, s.store.Leaderboard(10, false)))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// rankedTicket gives a signed-in game a one-time ticket for the relay's ranked queue.
func (s *Server) rankedTicket(w http.ResponseWriter, r *http.Request) {
	a, ok := s.store.Session(bearer(r), s.now())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	t := s.store.NewTicket(a.ID, s.now())
	if r.URL.Query().Get("format") == "text" { // for the game: the ticket alone
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, t+"\n")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket": t, "expires_in": int(TicketLifetime / time.Second)})
}

func (s *Server) leaderboard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": s.store.Leaderboard(10, false), "min_matches": LeaderboardMin})
}

// Internal is the bot's view, on its own local listener (never behind the public proxy).
func (s *Server) Internal() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/discord/{id}", func(w http.ResponseWriter, r *http.Request) {
		st, ok := s.store.StatsOfDiscord(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no account"})
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	// The relay, for ranked play: a ticket's account, and the ranked matches it starts.
	mux.HandleFunc("POST /internal/ranked/redeem", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Ticket string `json:"ticket"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		a, rt, ok := s.store.RedeemTicket(in.Ticket, s.now())
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such ticket"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"account": a.ID, "name": a.Name, "rating": rt.Rating, "deviation": rt.Deviation})
	})
	mux.HandleFunc("POST /internal/ranked/pairing", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Key   string `json:"key"`
			Seats []struct {
				Slot    int   `json:"slot"`
				Account int64 `json:"account"`
			} `json:"seats"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		seats := map[int]int64{}
		for _, st := range in.Seats {
			seats[st.Slot] = st.Account
		}
		if len(seats) != len(in.Seats) || !s.store.Pair(in.Key, seats, s.now()) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pairing not taken"})
			return
		}
		log.Printf("accounts: ranked match %s paired", in.Key)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /internal/leaderboard", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"entries": s.store.Leaderboard(10, true), "min_matches": LeaderboardMin})
	})
	return mux
}

// Settle counts finished matches; the service calls it every few seconds.
func (s *Server) Settle() {
	if counted, dropped := s.store.Settle(s.now()); counted+dropped > 0 {
		log.Printf("accounts: %d match(es) counted, %d dropped (reports disagreed)", counted, dropped)
	}
}

func (s *Server) privacy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, htmlHead("Privacy")+privacyText+"</main></body></html>")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func page(w http.ResponseWriter, code int, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	io.WriteString(w, htmlHead(title)+"<h1>"+html.EscapeString(title)+"</h1><p>"+html.EscapeString(text)+
		"</p></main></body></html>")
}

func htmlHead(title string) string {
	return `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>` + html.EscapeString(title) + ` · FFNY Online</title><style>` +
		`body{margin:0;background:#111;color:#eee;font:17px/1.5 system-ui,sans-serif}` +
		`main{max-width:640px;margin:12vh auto;padding:0 16px}h1{color:#f28c28;font-size:1.6em}` +
		`a{color:#f28c28}small{color:#999}</style></head><body><main>` +
		`<p><small>FFNY Online</small></p>`
}

const privacyText = `<h1>Privacy</h1>
<p>FFNY Online is an unofficial, non-commercial fan project. Signing in is optional; online play works without an account.</p>
<h2>What is stored when you sign in with Discord</h2>
<ul><li>your Discord user id, username, display name and avatar id, so the game can show who you are;</li>
<li>when the account was made and last used;</li>
<li>a session for each launcher you signed in with (stored only as a hash);</li>
<li>the results of the online matches you play while signed in: wins, losses, the fighters you used, and your last 20 matches with the venue and your opponents' names.</li></ul>
<p>Your statistics are shown to you in the game, and your name, wins and losses on the public leaderboard (in the game and on the project's Discord, where members can also look up your statistics with /profile).</p>
<p>Nothing else is read from Discord (only the <em>identify</em> permission), and Discord's own login token is not kept.
The data is stored on the project's server in Germany and is not shared or sold.</p>
<h2>Deleting it</h2>
<p>Sign out in the launcher to end its session. <em>Delete account</em> in the launcher removes the account and all its sessions at once.
You can also ask in a ticket on the project's Discord.</p>
<p><a href="https://github.com/sweeax-exe/ffny-online">github.com/sweeax-exe/ffny-online</a></p>`
