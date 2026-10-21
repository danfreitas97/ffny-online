package accounts

import (
	"math"
	"testing"
)

// The worked example of Glickman's "Example of the Glicko-2 system": a 1500/200 player beats a
// 1400/30 player and loses to 1550/100 and 1700/300 in one period.
func TestGlickmanExample(t *testing.T) {
	p := Rating{Rating: 1500, Deviation: 200, Volatility: 0.06}
	got := p.Update([]GameResult{
		{Opponent: Rating{Rating: 1400, Deviation: 30, Volatility: 0.06}, Score: 1},
		{Opponent: Rating{Rating: 1550, Deviation: 100, Volatility: 0.06}, Score: 0},
		{Opponent: Rating{Rating: 1700, Deviation: 300, Volatility: 0.06}, Score: 0},
	})
	if math.Abs(got.Rating-1464.06) > 0.05 || math.Abs(got.Deviation-151.52) > 0.05 ||
		math.Abs(got.Volatility-0.05999) > 0.00001 {
		t.Fatalf("got %+v, want 1464.06 / 151.52 / 0.05999", got)
	}
}

func TestOneGame(t *testing.T) {
	a, b := Game(NewRating(), NewRating(), 1)
	if !(a.Rating > RatingStart && b.Rating < RatingStart) || math.Abs((a.Rating-RatingStart)+(b.Rating-RatingStart)) > 1e-6 {
		t.Fatalf("even new players: %+v %+v", a, b)
	}
	if a.Deviation >= DeviationStart || b.Deviation >= DeviationStart {
		t.Fatal("a game makes the system surer")
	}
	// Beating a much stronger, settled player is worth more than beating an equal one.
	strong := Rating{Rating: 1900, Deviation: 60, Volatility: 0.06}
	equal := Rating{Rating: 1500, Deviation: 60, Volatility: 0.06}
	me := Rating{Rating: 1500, Deviation: 60, Volatility: 0.06}
	upset, _ := Game(me, strong, 1)
	plain, _ := Game(me, equal, 1)
	if upset.Rating-me.Rating <= plain.Rating-me.Rating {
		t.Fatalf("upset +%.1f, plain +%.1f", upset.Rating-me.Rating, plain.Rating-me.Rating)
	}
	// A settled player moves less than a new one.
	settledWin, _ := Game(me, equal, 1)
	newWin, _ := Game(Rating{Rating: 1500, Deviation: 350, Volatility: 0.06}, equal, 1)
	if newWin.Rating-1500 <= settledWin.Rating-1500 {
		t.Fatal("a new player should move faster")
	}
	if d, _ := Game(Rating{Rating: 1500, Deviation: 45, Volatility: 0.06}, equal, 1); d.Deviation < DeviationFloor {
		t.Fatalf("deviation below the floor: %.1f", d.Deviation)
	}
	// Not seen for a period: only the deviation grows, up to the start value.
	idle := equal.Update(nil)
	if idle.Rating != equal.Rating || idle.Deviation <= equal.Deviation || NewRating().Update(nil).Deviation > DeviationStart {
		t.Fatalf("idle %+v", idle)
	}
}
