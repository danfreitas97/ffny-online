// Package accounts is FFNY Online's account service: sign-in with Discord for the launcher,
// the accounts it makes and the launcher's sessions. Standard library only, like the relay.
//
// The store keeps everything in memory and writes it to one JSON file (atomically, a moment
// after a change). That is plenty for the number of players a fan project has, and keeps the
// server free of a database to run and update.
package accounts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Account is one player. Only what the game shows is kept: Discord's id, names and avatar.
type Account struct {
	ID        int64        `json:"id"`
	DiscordID string       `json:"discord_id"`
	Username  string       `json:"username"`
	Name      string       `json:"name"`             // what the game and the leaderboard show: Nick, else Global, else Username
	Global    string       `json:"global,omitempty"` // Discord's display name (empty: none, or an account from before)
	Nick      string       `json:"nick,omitempty"`   // the nickname on the FFNY Online Discord server
	Avatar    string       `json:"avatar,omitempty"`
	Created   time.Time    `json:"created"`
	LastSeen  time.Time    `json:"last_seen"`
	Stats     *Stats       `json:"stats,omitempty"`
	Ranked    *RankedStats `json:"ranked,omitempty"`
}

type session struct {
	Account  int64     `json:"account"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`
}

type fileData struct {
	NextID   int64               `json:"next_id"`
	Accounts []*Account          `json:"accounts"`
	Sessions map[string]*session `json:"sessions"` // key: hex SHA-256 of the token
}

// Store holds the accounts and sessions.
type Store struct {
	mu        sync.Mutex
	path      string
	nextID    int64
	accounts  map[int64]*Account
	byDiscord map[string]int64
	sessions  map[string]*session
	pending   map[string]*pendingMatch  // match reports waiting for the others (not saved)
	tickets   map[string]ticket         // ranked tickets by hash (not saved)
	pairings  map[string]*rankedPairing // ranked matches the relay started (not saved)
	pairDays  map[string]int            // ranked games per pair today (not saved)
	drops     []string                  // why matches were dropped since the last TakeDrops (not saved)
	dirty     bool
}

// SessionLifetime is how long a launcher stays signed in without using its session.
const SessionLifetime = 180 * 24 * time.Hour

// Open loads the store from path (a missing file is an empty store).
func Open(path string) (*Store, error) {
	s := &Store{path: path, nextID: 1, accounts: map[int64]*Account{}, byDiscord: map[string]int64{},
		sessions: map[string]*session{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var d fileData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if d.NextID > s.nextID {
		s.nextID = d.NextID
	}
	for _, a := range d.Accounts {
		s.accounts[a.ID] = a
		s.byDiscord[a.DiscordID] = a.ID
	}
	for k, v := range d.Sessions {
		s.sessions[k] = v
	}
	return s, nil
}

// Save writes the store if it changed since the last save.
func (s *Store) Save() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	d := fileData{NextID: s.nextID, Sessions: map[string]*session{}}
	for _, a := range s.accounts {
		c := *a
		if a.Stats != nil {
			st := *a.Stats
			st.Fighters, st.FNames = copyMap(a.Stats.Fighters), copyMap(a.Stats.FNames)
			st.Recent = append([]MatchLine(nil), a.Stats.Recent...)
			c.Stats = &st
		}
		if a.Ranked != nil {
			r := *a.Ranked
			c.Ranked = &r
		}
		d.Accounts = append(d.Accounts, &c)
	}
	for k, v := range s.sessions {
		c := *v
		d.Sessions[k] = &c
	}
	s.dirty = false
	s.mu.Unlock()
	raw, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".new"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Upsert makes or updates the account for a Discord user and returns a copy.
func (s *Store) Upsert(discordID, username, name, avatar string, now time.Time) Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byDiscord[discordID]
	if !ok {
		id = s.nextID
		s.nextID++
		s.accounts[id] = &Account{ID: id, DiscordID: discordID, Created: now}
		s.byDiscord[discordID] = id
	}
	a := s.accounts[id]
	a.Username, a.Global, a.Avatar, a.LastSeen = username, name, avatar, now
	a.Name = a.shownName()
	s.dirty = true
	return *a
}

// shownName is the server nickname, else the display name, else the username.
func (a *Account) shownName() string {
	switch {
	case a.Nick != "":
		return a.Nick
	case a.Global != "":
		return a.Global
	case a.Username != "":
		return a.Username
	}
	return a.Name
}

// SetNick records the account's nickname on our Discord server ("" when it has none or left) and returns the
// account and whether its shown name changed.
func (s *Store) SetNick(id int64, nick string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return Account{}, false
	}
	if a.Global == "" && a.Nick == "" && a.Name != a.Username {
		a.Global = a.Name // an account from before display names were kept apart
	}
	old, oldNick := a.Name, a.Nick
	a.Nick = nick
	a.Name = a.shownName()
	if a.Name != old || a.Nick != oldNick {
		s.dirty = true
	}
	return *a, a.Name != old
}

// RecentAccounts lists the ids of the accounts seen since a time (for the nickname refresh).
func (s *Store) RecentAccounts(since time.Time) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for id, a := range s.accounts {
		if !a.LastSeen.Before(since) {
			ids = append(ids, id)
		}
	}
	return ids
}

// DiscordIDOf returns an account's Discord user id.
func (s *Store) DiscordIDOf(id int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.accounts[id]; ok {
		return a.DiscordID
	}
	return ""
}

// NewSession gives an account a session and returns its token (shown once, stored hashed).
func (s *Store) NewSession(account int64, now time.Time) string {
	token := randomHex(32)
	s.mu.Lock()
	s.sessions[hashHex(token)] = &session{Account: account, Created: now, LastUsed: now}
	s.dirty = true
	s.mu.Unlock()
	return token
}

// Session returns the account behind a token, or false.
func (s *Store) Session(token string, now time.Time) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hashHex(token)
	ses, ok := s.sessions[key]
	if !ok {
		return Account{}, false
	}
	if now.Sub(ses.LastUsed) > SessionLifetime {
		delete(s.sessions, key)
		s.dirty = true
		return Account{}, false
	}
	a, ok := s.accounts[ses.Account]
	if !ok {
		delete(s.sessions, key)
		s.dirty = true
		return Account{}, false
	}
	if now.Sub(ses.LastUsed) > time.Hour { // keep the file quiet: once an hour is enough
		ses.LastUsed = now
		a.LastSeen = now
		s.dirty = true
	}
	return *a, true
}

// EndSession signs one launcher out.
func (s *Store) EndSession(token string) {
	s.mu.Lock()
	if _, ok := s.sessions[hashHex(token)]; ok {
		delete(s.sessions, hashHex(token))
		s.dirty = true
	}
	s.mu.Unlock()
}

// Delete removes an account and every session of it.
func (s *Store) Delete(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return
	}
	delete(s.byDiscord, a.DiscordID)
	delete(s.accounts, id)
	for k, v := range s.sessions {
		if v.Account == id {
			delete(s.sessions, k)
		}
	}
	s.dirty = true
}

// Count is the number of accounts.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts)
}

func copyMap[V any](m map[string]V) map[string]V {
	if m == nil {
		return nil
	}
	c := make(map[string]V, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
