# Ranked play: design

FFNY Online's ranked mode, collected from the owner and the community on the Discord
(2026-10-21) and written down before it is built. Nothing here is final until it ships.

## What players see

- **RANKED MATCH** in the ONLINE menu, beside QUICK MATCH. One on One only.
- **Search**: the server looks for an opponent with a similar rating; the window starts narrow
  (about ±100 points) and widens the longer the search runs, so a match comes even when few
  play. Later, with servers in several regions, the search prefers a good ping.
- **Fight the CPU while searching**: the search runs in the background of an offline match
  against the CPU. When an opponent is found, both get **OPPONENT FOUND – ACCEPT?** with a
  countdown; only when both accept does the CPU match end and the ranked match start. Whoever
  declines or lets it run out goes back to searching (or out of the queue); the other keeps
  searching.
- **Sets**: a ranked match is a set (first to 2 to begin with, at most first to 5).
  **Loser picks the venue** for the next game of the set.
- **Rematch** after a set when both want it. Repeated sets against the same opponent give fewer
  points (from the third set of a day half, later none), so friends cannot farm each other.
- **Ranks**: 30 to 40 ranks from the bottom to the top, the last five clearly harder to reach.
  Names from the community (examples: Street Rookie, Hustler, Contender, Veteran, Kingpin,
  Lord, Emperor, Def Jam Legend). Badges are the project's own artwork, no EA or Def Jam marks.
- **Points and progress**: after every ranked set a results page shows the rank badge, the
  points won or lost (**+18** / **−12**) and a bar to the next rank; promotion and demotion get
  their own moment. A short protection after a promotion keeps one loss from demoting at once.
- **Ranks everywhere**: next to names in rooms, on MATCH FOUND, on a **versus loading screen**
  (both names, ranks, fighters, records) and briefly in the fighters' intro, where a player one
  win from promotion shows **PROMOTION MATCH** (blinking) and one loss from demotion
  **DEMOTION MATCH**. MY STATS shows rank, points and the bar; the Discord shows it in `/profile`,
  `/leaderboard` switches to the ranked ladder, and rank roles can follow the rank.

## How it works

- **Rating**: Glicko-2 per account (rating, deviation, volatility), updated after every ranked
  game. Visible rank points are derived from it, so a new player moves fast and a settled one
  slowly, and a win against a stronger opponent is worth more.
- **Trust**: a ranked game counts only when the relay paired the two accounts for it **and**
  both games report the same result. The relay asks the account service, on the same server,
  for the accounts behind the players' sessions; the game never states its own rating.
- **Queue**: in the relay (it already pairs players and starts matches). A `RANKED` request
  carries the player's session token; the relay checks it with the account service's local
  listener, keeps a queue ordered by rating and search time, pairs, and starts a private room
  with fixed rules.
- **Results**: the account service gets the pairing from the relay and the two reports from
  the games; agreeing reports update both ratings, disagreeing ones count for nobody.
- **Seasons**: later; the ladder can be reset softly (ratings pulled toward the middle).

## Order of work

1. Rating and ranks in the account service (Glicko-2, tests), the ranked leaderboard.
2. The relay's ranked queue and pairing, with the account check.
3. The game: RANKED MATCH, search, accept, sets, loser picks, the results page with the bar.
4. Versus screen, ranks next to names, promotion/demotion in the intro, MY STATS, Discord.
5. CPU training while searching, rematch with diminishing points.
