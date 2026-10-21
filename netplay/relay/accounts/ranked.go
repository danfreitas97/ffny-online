package accounts

import (
	"strconv"
	"strings"
	"time"
)

// Ranked play (docs/ranked-design.md). The game asks for a ticket (POST /api/ranked/ticket) and
// hands it to the relay with its RANKED request; the relay redeems it on the internal listener,
// so the launcher's session never travels over UDP and the relay learns only the account. When
// the relay starts a ranked match it posts the pairing (room code and seed, the key the games
// report under). A ranked game counts once both paired accounts reported it and the reports
// agree (Settle); then both ratings move, Glicko-2, each game its own rating period.

const (
	// TicketLifetime is how long a ranked ticket can be redeemed (once).
	TicketLifetime = time.Minute
	// PairingLifetime is how long a pairing waits for its games' reports (a set, rematches).
	PairingLifetime = 3 * time.Hour
	// RankedPairDaily is how many ranked games between the same two accounts move their
	// ratings in one day (UTC); later games still count as played, so friends cannot farm
	// each other.
	RankedPairDaily = 5
)

// RankedStats is an account's ranked record.
type RankedStats struct {
	Rating Rating    `json:"rating"`
	Games  int       `json:"games"`
	Wins   int       `json:"wins"`
	Losses int       `json:"losses"`
	Draws  int       `json:"draws"`
	Last   time.Time `json:"last"`
}

// PublicRanked is the ranked record as the game and the bot show it.
type PublicRanked struct {
	Rating    int `json:"rating"`
	Deviation int `json:"deviation"`
	Games     int `json:"games"`
	Wins      int `json:"wins"`
	Losses    int `json:"losses"`
	Draws     int `json:"draws"`
}

type ticket struct {
	account int64
	expires time.Time
}

type rankedPairing struct {
	seats map[int]int64 // slot -> account
	at    time.Time
}

func (a *Account) rating() Rating {
	if a.Ranked == nil {
		return NewRating()
	}
	return a.Ranked.Rating
}

func (a *Account) publicRanked() *PublicRanked {
	if a.Ranked == nil {
		return nil
	}
	r := a.Ranked
	return &PublicRanked{Rating: int(r.Rating.Rating + 0.5), Deviation: int(r.Rating.Deviation + 0.5),
		Games: r.Games, Wins: r.Wins, Losses: r.Losses, Draws: r.Draws}
}

// NewTicket gives the account a one-time ranked ticket (32 hex digits, 16 bytes).
func (s *Store) NewTicket(account int64, now time.Time) string {
	t := randomHex(16)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tickets == nil {
		s.tickets = map[string]ticket{}
	}
	for k, v := range s.tickets {
		if now.After(v.expires) {
			delete(s.tickets, k)
		}
	}
	s.tickets[hashHex(t)] = ticket{account: account, expires: now.Add(TicketLifetime)}
	return t
}

// RedeemTicket returns the account behind a ticket and its rating, and spends the ticket.
func (s *Store) RedeemTicket(t string, now time.Time) (Account, Rating, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hashHex(strings.ToLower(t))
	tk, ok := s.tickets[key]
	delete(s.tickets, key)
	if !ok || now.After(tk.expires) {
		return Account{}, Rating{}, false
	}
	a, ok := s.accounts[tk.account]
	if !ok {
		return Account{}, Rating{}, false
	}
	return *a, a.rating(), true
}

// Pair records a ranked match the relay started: key is the room code and seed the games
// report under ("ABC123-1A2B3C4D"), seats the account in each slot.
func (s *Store) Pair(key string, seats map[int]int64, now time.Time) bool {
	if len(key) != 15 || key[6] != '-' || len(seats) != 2 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[int64]bool{}
	for slot, id := range seats {
		if slot < 0 || slot > 3 || seen[id] || s.accounts[id] == nil {
			return false
		}
		seen[id] = true
	}
	if s.pairings == nil {
		s.pairings = map[string]*rankedPairing{}
	}
	s.pairings[key] = &rankedPairing{seats: seats, at: now}
	return true
}

// pairingOf is the pairing a report key belongs to: the key without its match number.
func pairingKeyOf(reportKey string) string {
	if len(reportKey) >= 15 && reportKey[6] == '-' && (len(reportKey) == 15 || reportKey[15] == '-') {
		return reportKey[:15]
	}
	return ""
}

// rankedResult moves the ratings for a settled match when it is a ranked game: the relay
// paired exactly the two accounts that reported it, each from its own seat. Called with s.mu
// held, from Settle.
func (s *Store) rankedResult(key string, p *pendingMatch, now time.Time) bool {
	rp := s.pairings[pairingKeyOf(key)]
	if rp == nil || len(p.reports) != 2 {
		return false
	}
	var ids [2]int64
	var r MatchReport
	i := 0
	for id, rep := range p.reports {
		if rp.seats[rep.You] != id {
			return false
		}
		ids[i], r = id, rep
		i++
	}
	if len(r.Players) != 2 {
		return false
	}
	for _, pl := range r.Players {
		if pl.CPU || rp.seats[pl.Slot] == 0 {
			return false
		}
	}
	a, b := s.accounts[ids[0]], s.accounts[ids[1]]
	if a == nil || b == nil {
		return false
	}
	slotOf := func(id int64) int {
		for slot, acc := range rp.seats {
			if acc == id {
				return slot
			}
		}
		return -1
	}
	scoreA := 0.5
	if !r.Draw {
		scoreA = 0
		for _, w := range r.Winners {
			if w == slotOf(a.ID) {
				scoreA = 1
			}
		}
	}

	if s.pairDays == nil {
		s.pairDays = map[string]int{}
	}
	lo, hi := min(a.ID, b.ID), max(a.ID, b.ID)
	day := p.first.UTC().Format("2006-01-02")
	pk := day + "/" + strconv.FormatInt(lo, 10) + "/" + strconv.FormatInt(hi, 10)
	for k := range s.pairDays {
		if !strings.HasPrefix(k, day) {
			delete(s.pairDays, k)
		}
	}
	s.pairDays[pk]++
	moves := s.pairDays[pk] <= RankedPairDaily

	for _, acc := range []*Account{a, b} {
		if acc.Ranked == nil {
			acc.Ranked = &RankedStats{Rating: NewRating()}
		}
	}
	if moves {
		a.Ranked.Rating, b.Ranked.Rating = Game(a.Ranked.Rating, b.Ranked.Rating, scoreA)
	}
	for _, x := range []struct {
		acc   *Account
		score float64
	}{{a, scoreA}, {b, 1 - scoreA}} {
		st := x.acc.Ranked
		st.Games++
		st.Last = p.first
		switch x.score {
		case 1:
			st.Wins++
		case 0:
			st.Losses++
		default:
			st.Draws++
		}
	}
	return true
}

// expirePairings forgets pairings whose games can no longer be reported. Called with s.mu held.
func (s *Store) expirePairings(now time.Time) {
	for k, p := range s.pairings {
		if now.Sub(p.at) > PairingLifetime {
			delete(s.pairings, k)
		}
	}
}
