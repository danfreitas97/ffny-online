package accounts

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Match results and the statistics made from them.
//
// Every signed-in player's game reports the match it just played (POST /api/matches). The
// reports of one match share a key (room, seed and match id). A match counts once the reports
// have had ReportWindow to arrive and all of them agree on who played what and who won; then each
// reporter's own line goes into their statistics. Reports that disagree are dropped. Casual
// statistics trust the games; ranked play will add the relay's word that the match took place.

// ReportWindow is how long the other players' reports may take after the first one.
const ReportWindow = 45 * time.Second

// RecentMatches is how many matches an account keeps in its history.
const RecentMatches = 20

const maxReportsPerHour = 40

// MatchPlayer is one fighter of a reported match.
type MatchPlayer struct {
	Slot    int    `json:"slot"`
	Name    string `json:"name"`
	Fighter int    `json:"fighter"`
	FName   string `json:"fighter_name"` // from the player's own dump; the project ships no names
	CPU     bool   `json:"cpu"`
}

// MatchReport is what a game sends after a match.
type MatchReport struct {
	Key       string        `json:"key"`
	Mode      int           `json:"mode"`
	ModeName  string        `json:"mode_name"`
	Venue     int           `json:"venue"`
	VenueName string        `json:"venue_name"`
	You       int           `json:"you"` // the reporter's slot
	Players   []MatchPlayer `json:"players"`
	Winners   []int         `json:"winners"`
	Draw      bool          `json:"draw"`
}

// MatchLine is one match in an account's history.
type MatchLine struct {
	At        time.Time `json:"at"`
	Result    string    `json:"result"` // win, loss, draw
	Mode      string    `json:"mode"`
	Venue     string    `json:"venue"`
	Fighter   string    `json:"fighter"`
	Opponents []string  `json:"opponents"`
}

// Stats are an account's numbers.
type Stats struct {
	Matches    int               `json:"matches"`
	Wins       int               `json:"wins"`
	Losses     int               `json:"losses"`
	Draws      int               `json:"draws"`
	Streak     int               `json:"streak"` // wins in a row now
	BestStreak int               `json:"best_streak"`
	Fighters   map[string]int    `json:"fighters,omitempty"` // fighter id -> matches
	FNames     map[string]string `json:"fighter_names,omitempty"`
	Recent     []MatchLine       `json:"recent,omitempty"`
	reports    []time.Time
}

// valid checks a report's shape; the outcome itself is checked against the other reports.
func (r *MatchReport) valid() bool {
	if len(r.Key) < 8 || len(r.Key) > 80 || len(r.Players) < 2 || len(r.Players) > 4 || r.Mode < 0 || r.Mode > 255 ||
		r.Venue < 0 || r.Venue > 255 || len(r.ModeName) > 40 || len(r.VenueName) > 60 {
		return false
	}
	slots := map[int]bool{}
	for _, p := range r.Players {
		if p.Slot < 0 || p.Slot > 3 || slots[p.Slot] || p.Fighter < 0 || p.Fighter > 255 || len(p.Name) > 32 || len(p.FName) > 40 {
			return false
		}
		slots[p.Slot] = true
	}
	if !slots[r.You] {
		return false
	}
	for _, w := range r.Winners {
		if !slots[w] {
			return false
		}
	}
	return r.Draw || len(r.Winners) > 0
}

// outcome is what every report of one match must agree on.
func (r *MatchReport) outcome() string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(r.Mode) + "/" + strconv.Itoa(r.Venue) + "/" + strconv.FormatBool(r.Draw) + "/")
	ps := append([]MatchPlayer(nil), r.Players...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Slot < ps[j].Slot })
	for _, p := range ps {
		b.WriteString(strconv.Itoa(p.Slot) + ":" + strconv.Itoa(p.Fighter) + ":" + strconv.FormatBool(p.CPU) + ",")
	}
	ws := append([]int(nil), r.Winners...)
	sort.Ints(ws)
	for _, w := range ws {
		b.WriteString("w" + strconv.Itoa(w))
	}
	return b.String()
}

type pendingMatch struct {
	first   time.Time
	reports map[int64]MatchReport // by account
}

// Report takes a game's report. It returns false when the report is malformed or the account
// sends too many.
func (s *Store) Report(account int64, r MatchReport, now time.Time) bool {
	if !r.valid() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[account]
	if !ok {
		return false
	}
	st := a.stats()
	recent := st.reports[:0]
	for _, t := range st.reports {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	st.reports = recent
	if len(recent) >= maxReportsPerHour {
		return false
	}
	st.reports = append(st.reports, now)
	if s.pending == nil {
		s.pending = map[string]*pendingMatch{}
	}
	p, ok := s.pending[r.Key]
	if !ok {
		p = &pendingMatch{first: now, reports: map[int64]MatchReport{}}
		s.pending[r.Key] = p
	}
	if _, dup := p.reports[account]; !dup {
		p.reports[account] = r
	}
	return true
}

// Settle counts the matches whose report window is over. It returns how many counted and how
// many were dropped because their reports disagreed.
func (s *Store) Settle(now time.Time) (counted, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, p := range s.pending {
		if now.Sub(p.first) < ReportWindow {
			continue
		}
		delete(s.pending, key)
		var want string
		agree := true
		for _, r := range p.reports {
			if want == "" {
				want = r.outcome()
			} else if r.outcome() != want {
				agree = false
			}
		}
		if !agree {
			dropped++
			continue
		}
		for id, r := range p.reports {
			if a, ok := s.accounts[id]; ok {
				a.stats().add(r, p.first)
			}
		}
		s.rankedResult(key, p, now)
		counted++
		s.dirty = true
	}
	s.expirePairings(now)
	return counted, dropped
}

func (a *Account) stats() *Stats {
	if a.Stats == nil {
		a.Stats = &Stats{}
	}
	return a.Stats
}

func (st *Stats) add(r MatchReport, at time.Time) {
	line := MatchLine{At: at, Mode: r.ModeName, Venue: r.VenueName}
	won := false
	for _, w := range r.Winners {
		if w == r.You {
			won = true
		}
	}
	for _, p := range r.Players {
		if p.Slot == r.You {
			line.Fighter = p.FName
			if st.Fighters == nil {
				st.Fighters, st.FNames = map[string]int{}, map[string]string{}
			}
			id := strconv.Itoa(p.Fighter)
			st.Fighters[id]++
			if p.FName != "" {
				st.FNames[id] = p.FName
			}
		} else {
			name := p.Name
			if p.CPU {
				name = "CPU"
			}
			line.Opponents = append(line.Opponents, name)
		}
	}
	st.Matches++
	switch {
	case r.Draw:
		st.Draws++
		line.Result = "draw"
		st.Streak = 0
	case won:
		st.Wins++
		line.Result = "win"
		st.Streak++
		if st.Streak > st.BestStreak {
			st.BestStreak = st.Streak
		}
	default:
		st.Losses++
		line.Result = "loss"
		st.Streak = 0
	}
	st.Recent = append([]MatchLine{line}, st.Recent...)
	if len(st.Recent) > RecentMatches {
		st.Recent = st.Recent[:RecentMatches]
	}
}

// PublicStats is an account's statistics as the game and the bot show them.
type PublicStats struct {
	Account      PublicAccount `json:"account"`
	Matches      int           `json:"matches"`
	Wins         int           `json:"wins"`
	Losses       int           `json:"losses"`
	Draws        int           `json:"draws"`
	WinRate      int           `json:"win_rate"` // percent of decided matches
	Streak       int           `json:"streak"`
	BestStreak   int           `json:"best_streak"`
	Favorite     int           `json:"favorite"` // fighter id, -1 none
	FavoriteName string        `json:"favorite_name"`
	Rank         int           `json:"rank"` // place on the leaderboard, 0 not on it
	Ranked       *PublicRanked `json:"ranked,omitempty"`
	Recent       []MatchLine   `json:"recent"`
}

// LeaderboardMin is how many matches put an account on the leaderboard.
const LeaderboardMin = 3

func (s *Store) publicStats(a *Account, rank int) PublicStats {
	st := a.stats()
	p := PublicStats{Account: public(*a), Matches: st.Matches, Wins: st.Wins, Losses: st.Losses, Draws: st.Draws,
		Streak: st.Streak, BestStreak: st.BestStreak, Favorite: -1, Rank: rank, Recent: st.Recent, Ranked: a.publicRanked()}
	if p.Recent == nil {
		p.Recent = []MatchLine{}
	}
	if d := st.Wins + st.Losses; d > 0 {
		p.WinRate = (st.Wins*100 + d/2) / d
	}
	best := 0
	for id, n := range st.Fighters {
		f, _ := strconv.Atoi(id)
		if n > best || (n == best && f < p.Favorite) {
			best, p.Favorite, p.FavoriteName = n, f, st.FNames[id]
		}
	}
	return p
}

// ranked is the leaderboard's order: most wins, then the better win rate, then fewer matches.
func (s *Store) ranked() []*Account {
	var list []*Account
	for _, a := range s.accounts {
		if a.Stats != nil && a.Stats.Matches >= LeaderboardMin {
			list = append(list, a)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		x, y := list[i].Stats, list[j].Stats
		if x.Wins != y.Wins {
			return x.Wins > y.Wins
		}
		rx, ry := x.Wins*1000/max(1, x.Wins+x.Losses), y.Wins*1000/max(1, y.Wins+y.Losses)
		if rx != ry {
			return rx > ry
		}
		if x.Matches != y.Matches {
			return x.Matches < y.Matches
		}
		return list[i].ID < list[j].ID
	})
	return list
}

// StatsOf is an account's statistics with its place on the leaderboard.
func (s *Store) StatsOf(id int64) (PublicStats, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return PublicStats{}, false
	}
	rank := 0
	for i, r := range s.ranked() {
		if r.ID == id {
			rank = i + 1
		}
	}
	return s.publicStats(a, rank), true
}

// StatsOfDiscord is StatsOf for a Discord user id.
func (s *Store) StatsOfDiscord(discordID string) (PublicStats, bool) {
	s.mu.Lock()
	id, ok := s.byDiscord[discordID]
	s.mu.Unlock()
	if !ok {
		return PublicStats{}, false
	}
	return s.StatsOf(id)
}

// LeaderboardEntry is one line of the leaderboard.
type LeaderboardEntry struct {
	Rank    int    `json:"rank"`
	Name    string `json:"name"`
	Wins    int    `json:"wins"`
	Losses  int    `json:"losses"`
	Matches int    `json:"matches"`
	WinRate int    `json:"win_rate"`
	Discord string `json:"discord_id,omitempty"` // internal listing only
}

// Leaderboard is the first n places.
func (s *Store) Leaderboard(n int, withDiscord bool) []LeaderboardEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []LeaderboardEntry{}
	for i, a := range s.ranked() {
		if i >= n {
			break
		}
		p := s.publicStats(a, i+1)
		e := LeaderboardEntry{Rank: i + 1, Name: a.Name, Wins: p.Wins, Losses: p.Losses, Matches: p.Matches, WinRate: p.WinRate}
		if withDiscord {
			e.Discord = a.DiscordID
		}
		out = append(out, e)
	}
	return out
}

// StatsText is the game's form of an account's statistics and the leaderboard: one
// "key=value" per line, list entries as fields split by '|'. Names lose '|' and line breaks.
//
//	name=Masa
//	matches=12 / wins / losses / draws / win_rate / streak / best_streak / rank / min=3
//	favorite=55
//	favorite_name=...
//	recent=win|fighter|opponent, opponent|venue        (newest first)
//	board=1|Masa|9|2|82                                 (rank|name|wins|losses|win rate)
func StatsText(st PublicStats, board []LeaderboardEntry) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == '|' || r == '\n' || r == '\r' {
				return ' '
			}
			return r
		}, s)
	}
	var b strings.Builder
	line := func(k, v string) { b.WriteString(k + "=" + v + "\n") }
	num := func(k string, v int) { line(k, strconv.Itoa(v)) }
	line("name", clean(st.Account.Name))
	num("matches", st.Matches)
	num("wins", st.Wins)
	num("losses", st.Losses)
	num("draws", st.Draws)
	num("win_rate", st.WinRate)
	num("streak", st.Streak)
	num("best_streak", st.BestStreak)
	num("rank", st.Rank)
	num("min", LeaderboardMin)
	num("favorite", st.Favorite)
	line("favorite_name", clean(st.FavoriteName))
	if r := st.Ranked; r != nil {
		line("ranked", fmt.Sprintf("%d|%d|%d|%d|%d", r.Rating, r.Games, r.Wins, r.Losses, r.Draws))
	}
	for _, m := range st.Recent {
		opp := make([]string, len(m.Opponents))
		for i, o := range m.Opponents {
			opp[i] = clean(o)
		}
		line("recent", m.Result+"|"+clean(m.Fighter)+"|"+strings.Join(opp, ", ")+"|"+clean(m.Venue))
	}
	for _, e := range board {
		line("board", fmt.Sprintf("%d|%s|%d|%d|%d", e.Rank, clean(e.Name), e.Wins, e.Losses, e.WinRate))
	}
	return b.String()
}
