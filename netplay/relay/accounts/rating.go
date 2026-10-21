package accounts

import "math"

// Glicko-2 (Mark Glickman, "Example of the Glicko-2 system", 2013) for ranked play
// (docs/ranked-design.md). A rating on the familiar scale (1500 to start), its deviation (how
// sure the system is: 350 for a new player) and the volatility (how erratic the results are).

const (
	RatingStart     = 1500.0
	DeviationStart  = 350.0
	VolatilityStart = 0.06
	glickoScale     = 173.7178
	glickoTau       = 0.5 // how fast the volatility may change
	glickoEpsilon   = 0.000001
	// DeviationFloor keeps a settled player's rating able to move.
	DeviationFloor = 45.0
)

// Rating is one player's Glicko-2 state.
type Rating struct {
	Rating     float64 `json:"rating"`
	Deviation  float64 `json:"deviation"`
	Volatility float64 `json:"volatility"`
}

// NewRating is a new player's rating.
func NewRating() Rating {
	return Rating{Rating: RatingStart, Deviation: DeviationStart, Volatility: VolatilityStart}
}

// GameResult is one game against an opponent: Score 1 a win, 0 a loss, 0.5 a draw.
type GameResult struct {
	Opponent Rating
	Score    float64
}

func glickoG(phi float64) float64 { return 1 / math.Sqrt(1+3*phi*phi/(math.Pi*math.Pi)) }

func glickoE(mu, muj, phij float64) float64 { return 1 / (1 + math.Exp(-glickoG(phij)*(mu-muj))) }

// Update is the rating after one rating period with these games. With no games only the
// deviation grows (the player has not been seen).
func (r Rating) Update(games []GameResult) Rating {
	mu := (r.Rating - RatingStart) / glickoScale
	phi := r.Deviation / glickoScale
	sigma := r.Volatility
	if len(games) == 0 {
		phi = math.Min(math.Sqrt(phi*phi+sigma*sigma), DeviationStart/glickoScale)
		return Rating{Rating: r.Rating, Deviation: phi * glickoScale, Volatility: sigma}
	}
	var vInv, deltaSum float64
	for _, g := range games {
		muj := (g.Opponent.Rating - RatingStart) / glickoScale
		phij := g.Opponent.Deviation / glickoScale
		e := glickoE(mu, muj, phij)
		gj := glickoG(phij)
		vInv += gj * gj * e * (1 - e)
		deltaSum += gj * (g.Score - e)
	}
	v := 1 / vInv
	delta := v * deltaSum

	// The new volatility: the Illinois algorithm on f(x) (step 5 of the paper).
	a := math.Log(sigma * sigma)
	f := func(x float64) float64 {
		ex := math.Exp(x)
		d := phi*phi + v + ex
		return ex*(delta*delta-phi*phi-v-ex)/(2*d*d) - (x-a)/(glickoTau*glickoTau)
	}
	A := a
	var B float64
	if delta*delta > phi*phi+v {
		B = math.Log(delta*delta - phi*phi - v)
	} else {
		k := 1.0
		for f(a-k*glickoTau) < 0 {
			k++
		}
		B = a - k*glickoTau
	}
	fA, fB := f(A), f(B)
	for i := 0; math.Abs(B-A) > glickoEpsilon && i < 100; i++ {
		C := A + (A-B)*fA/(fB-fA)
		fC := f(C)
		if fC*fB <= 0 {
			A, fA = B, fB
		} else {
			fA /= 2
		}
		B, fB = C, fC
	}
	newSigma := math.Exp(A / 2)

	phiStar := math.Sqrt(phi*phi + newSigma*newSigma)
	newPhi := 1 / math.Sqrt(1/(phiStar*phiStar)+1/v)
	newMu := mu + newPhi*newPhi*deltaSum
	dev := math.Max(newPhi*glickoScale, DeviationFloor)
	return Rating{Rating: newMu*glickoScale + RatingStart, Deviation: dev, Volatility: newSigma}
}

// Game is the ratings of two players after one game between them (scoreA for player A: 1 win,
// 0 loss, 0.5 draw), each game its own rating period, as online ranked play does.
func Game(a, b Rating, scoreA float64) (Rating, Rating) {
	return a.Update([]GameResult{{Opponent: b, Score: scoreA}}), b.Update([]GameResult{{Opponent: a, Score: 1 - scoreA}})
}
