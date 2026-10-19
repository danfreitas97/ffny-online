package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/netip"
	"time"
	"unicode/utf8"
)

// Config holds the limits of the state machine. Zero values take the defaults
// from the specification (see setDefaults).
type Config struct {
	MaxRooms          int           // rooms open at once (default 512); beyond: ERROR 5
	MaxClients        int           // clients registered at once (default 1024); beyond: ERROR 5
	IdleTimeout       time.Duration // a client silent this long is removed (default 15 s)
	RoomExpiry        time.Duration // a room that never got a second player is deleted after this (default 10 min)
	HellosPerMinute   int           // HELLOs per source IP per sliding minute (default 10); beyond: ERROR 6
	MsgsPerSecond     int           // datagrams per source IP per second (default 400); beyond: silent drop
	MaxTrackedSources int           // size cap of the per-IP rate-limit table (default 100000)

	// Rand is the randomness source for room codes, tokens and the log salt.
	// Default: crypto/rand.Reader. Tests may inject a deterministic reader.
	Rand io.Reader

	// Logf receives one line per room event. Nil disables event logging.
	Logf func(format string, args ...any)
}

func (c *Config) setDefaults() {
	if c.MaxRooms <= 0 {
		c.MaxRooms = 512
	}
	if c.MaxClients <= 0 {
		c.MaxClients = 1024
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 15 * time.Second
	}
	if c.RoomExpiry <= 0 {
		c.RoomExpiry = 10 * time.Minute
	}
	if c.HellosPerMinute <= 0 {
		c.HellosPerMinute = 10
	}
	if c.MsgsPerSecond <= 0 {
		c.MsgsPerSecond = 400
	}
	if c.MaxTrackedSources <= 0 {
		c.MaxTrackedSources = 100000
	}
	if c.Rand == nil {
		c.Rand = rand.Reader
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
}

// Packet is one datagram the caller must send.
type Packet struct {
	To   netip.AddrPort
	Data []byte
}

// Stats are traffic counters since the last TakeStats call.
type Stats struct {
	PacketsIn, BytesIn   uint64 // every datagram received, valid or not
	PacketsOut, BytesOut uint64 // every datagram the relay asked to send
	// Dropped counts received datagrams that caused nothing to be sent:
	// malformed, rate-limited, unknown token after the first ERROR 7, DATA
	// while alone in the room, BYE of a lone player.
	Dropped uint64
}

// client is one registered player.
type client struct {
	addr     netip.AddrPort // source address and port; part of the identity
	token    Token
	slot     uint8
	name     string
	room     *room
	lastSeen time.Time
	id       string // anonymised address, for logs only
	welcome  []byte // the encoded WELCOME, re-sent on a repeated HELLO
}

// room pairs up to two clients.
type room struct {
	code     RoomCode
	build    Build
	slots    [2]*client
	created  time.Time
	everFull bool // a second player has joined at some point
}

// source is the rate-limit state of one source IP (IPv6: one /64).
type source struct {
	lastSeen time.Time

	// Fixed one-second window for the per-IP message limit.
	winStart time.Time
	winCount int

	// Ring of the times of the last HellosPerMinute counted HELLOs; the entry
	// at helloNext is the oldest.
	hellos    []time.Time
	helloNext int

	lastErr6 time.Time // ERROR 6 is sent at most once per second per source
	lastErr7 time.Time // ERROR 7 is sent at most once per second per source
}

// Relay is the room/client state machine. It is not safe for concurrent use;
// the Server serialises calls with a mutex. All methods take the current time
// as an argument, so tests can drive it with a fake clock.
type Relay struct {
	cfg     Config
	clients map[netip.AddrPort]*client
	rooms   map[RoomCode]*room
	sources map[netip.Addr]*source
	salt    [16]byte // per-process salt for anonymised log ids
	stats   Stats
	lastGC  time.Time
}

// NewRelay builds a state machine with the given limits.
func NewRelay(cfg Config) (*Relay, error) {
	cfg.setDefaults()
	r := &Relay{
		cfg:     cfg,
		clients: make(map[netip.AddrPort]*client),
		rooms:   make(map[RoomCode]*room),
		sources: make(map[netip.Addr]*source),
	}
	if _, err := io.ReadFull(cfg.Rand, r.salt[:]); err != nil {
		return nil, err
	}
	return r, nil
}

// Rooms returns the number of open rooms.
func (r *Relay) Rooms() int { return len(r.rooms) }

// Clients returns the number of registered clients.
func (r *Relay) Clients() int { return len(r.clients) }

// TakeStats returns the counters and resets them.
func (r *Relay) TakeStats() Stats {
	s := r.stats
	r.stats = Stats{}
	return s
}

// Handle processes one received datagram and returns the datagrams to send.
// b is not retained after Handle returns.
//
// Anything malformed is dropped without a reply. Every reply to an address
// that is not a registered client is no larger than the datagram that caused
// it, so the relay cannot be used to amplify traffic.
func (r *Relay) Handle(now time.Time, from netip.AddrPort, b []byte) []Packet {
	r.stats.PacketsIn++
	r.stats.BytesIn += uint64(len(b))

	out := r.handle(now, from, b)
	if len(out) == 0 {
		r.stats.Dropped++
	}
	r.countOut(out)
	return out
}

func (r *Relay) handle(now time.Time, from netip.AddrPort, b []byte) []Packet {
	if !from.IsValid() || from.Port() == 0 {
		return nil
	}
	src := r.source(now, from.Addr())
	if src == nil || !src.allowMsg(now, r.cfg.MsgsPerSecond) {
		return nil
	}
	msg, err := DecodeClient(b)
	if err != nil {
		return nil
	}
	switch m := msg.(type) {
	case Hello:
		return r.hello(now, from, src, m, len(b))
	case ClientData:
		c, out := r.auth(now, from, src, m.Token, len(b))
		if c == nil {
			return out
		}
		c.lastSeen = now
		peer := c.room.slots[1-c.slot]
		if peer == nil {
			return nil // nobody to forward to yet
		}
		data, err := Encode(ServerData{From: c.slot, Payload: m.Payload})
		if err != nil {
			return nil
		}
		return []Packet{{To: peer.addr, Data: data}}
	case Ping:
		c, out := r.auth(now, from, src, m.Token, len(b))
		if c == nil {
			return out
		}
		c.lastSeen = now
		return []Packet{{To: from, Data: mustEncode(Pong{Nonce: m.Nonce})}}
	case Bye:
		c, out := r.auth(now, from, src, m.Token, len(b))
		if c == nil {
			return out
		}
		return r.removeClient(c, "bye")
	}
	return nil
}

// hello handles HELLO: an idempotent re-send, a create, or a join.
func (r *Relay) hello(now time.Time, from netip.AddrPort, src *source, h Hello, reqLen int) []Packet {
	// A repeated HELLO from an address that already has a token gets the same
	// WELCOME (and the PEER, if the room is full, since that may have been lost
	// too). It is not counted against the HELLO rate limit.
	if c := r.clients[from]; c != nil {
		c.lastSeen = now
		out := []Packet{{To: from, Data: c.welcome}}
		if p := c.room.slots[1-c.slot]; p != nil {
			out = append(out, Packet{To: from, Data: mustEncode(Peer{Slot: p.slot, Name: p.name})})
		}
		return out
	}

	if !src.allowHello(now) {
		if now.Sub(src.lastErr6) < time.Second {
			return nil
		}
		src.lastErr6 = now
		return r.errorTo(from, ErrRateLimited, reqLen)
	}
	if !validName(h.Name) {
		return r.errorTo(from, ErrBadName, reqLen)
	}
	if len(r.clients) >= r.cfg.MaxClients {
		return r.errorTo(from, ErrServerFull, reqLen)
	}

	switch h.Mode {
	case ModeCreate:
		if len(r.rooms) >= r.cfg.MaxRooms {
			return r.errorTo(from, ErrServerFull, reqLen)
		}
		code, err := r.newRoomCode()
		if err != nil {
			return nil
		}
		rm := &room{code: code, build: h.Build, created: now}
		c, err := r.newClient(now, from, rm, SlotHost, h.Name)
		if err != nil {
			return nil
		}
		r.rooms[code] = rm
		r.cfg.Logf("room %s created by c=%s", code, c.id)
		return []Packet{{To: from, Data: c.welcome}}

	case ModeJoin:
		rm := r.rooms[h.Room]
		if rm == nil {
			return r.errorTo(from, ErrRoomNotFound, reqLen)
		}
		// HELLO mode 1 joins as slot 1 only. If the guest slot is taken the
		// room is full, even when the host has left (see README).
		if rm.slots[SlotGuest] != nil {
			return r.errorTo(from, ErrRoomFull, reqLen)
		}
		if subtle.ConstantTimeCompare(rm.build[:], h.Build[:]) != 1 {
			return r.errorTo(from, ErrBuildMismatch, reqLen)
		}
		c, err := r.newClient(now, from, rm, SlotGuest, h.Name)
		if err != nil {
			return nil
		}
		rm.everFull = true
		r.cfg.Logf("room %s joined by c=%s", rm.code, c.id)

		out := []Packet{{To: from, Data: c.welcome}}
		if host := rm.slots[SlotHost]; host != nil {
			out = append(out,
				Packet{To: from, Data: mustEncode(Peer{Slot: host.slot, Name: host.name})},
				Packet{To: host.addr, Data: mustEncode(Peer{Slot: c.slot, Name: c.name})},
			)
		}
		return out
	}
	return nil
}

// newClient registers a client in slot of rm with a fresh random token.
func (r *Relay) newClient(now time.Time, from netip.AddrPort, rm *room, slot uint8, name string) (*client, error) {
	c := &client{addr: from, slot: slot, name: name, room: rm, lastSeen: now, id: r.anon(from)}
	if _, err := io.ReadFull(r.cfg.Rand, c.token[:]); err != nil {
		return nil, err
	}
	c.welcome = mustEncode(Welcome{Token: c.token, Slot: slot, Room: rm.code})
	rm.slots[slot] = c
	r.clients[from] = c
	return c, nil
}

// auth finds the client that owns token at address from. A mismatch is
// answered with ERROR 7 at most once per second per source.
func (r *Relay) auth(now time.Time, from netip.AddrPort, src *source, tok Token, reqLen int) (*client, []Packet) {
	if c := r.clients[from]; c != nil && subtle.ConstantTimeCompare(c.token[:], tok[:]) == 1 {
		return c, nil
	}
	if now.Sub(src.lastErr7) < time.Second {
		return nil, nil
	}
	src.lastErr7 = now
	return nil, r.errorTo(from, ErrUnknownToken, reqLen)
}

// removeClient takes c out of its room, tells the remaining player (PEER with
// an empty name) and deletes the room if it is now empty.
func (r *Relay) removeClient(c *client, reason string) []Packet {
	delete(r.clients, c.addr)
	rm := c.room
	rm.slots[c.slot] = nil
	r.cfg.Logf("room %s peer left c=%s slot=%d reason=%s", rm.code, c.id, c.slot, reason)
	if other := rm.slots[1-c.slot]; other != nil {
		return []Packet{{To: other.addr, Data: mustEncode(Peer{Slot: c.slot, Name: ""})}}
	}
	delete(r.rooms, rm.code)
	r.cfg.Logf("room %s closed (empty)", rm.code)
	return nil
}

// Tick runs the timers: idle clients, rooms that never got a second player,
// and garbage collection of the per-IP table. Call it about once a second.
func (r *Relay) Tick(now time.Time) []Packet {
	var out []Packet
	for _, c := range r.clients {
		if now.Sub(c.lastSeen) >= r.cfg.IdleTimeout {
			out = append(out, r.removeClient(c, "timeout")...)
		}
	}
	for code, rm := range r.rooms {
		if rm.everFull || now.Sub(rm.created) < r.cfg.RoomExpiry {
			continue
		}
		for i, c := range rm.slots {
			if c != nil {
				delete(r.clients, c.addr)
				rm.slots[i] = nil
			}
		}
		delete(r.rooms, code)
		r.cfg.Logf("room %s expired (no second player after %s)", code, r.cfg.RoomExpiry)
	}
	if now.Sub(r.lastGC) >= 10*time.Second {
		r.lastGC = now
		for ip, s := range r.sources {
			// Older than the HELLO window: nothing left to remember.
			if now.Sub(s.lastSeen) > time.Minute+time.Second {
				delete(r.sources, ip)
			}
		}
	}
	r.countOut(out)
	return out
}

// ---- helpers ----

func (r *Relay) countOut(out []Packet) {
	for _, p := range out {
		r.stats.PacketsOut++
		r.stats.BytesOut += uint64(len(p.Data))
	}
}

// errorTo builds an ERROR reply. The text is left empty if the reply would
// otherwise be larger than the request (anti-amplification).
func (r *Relay) errorTo(to netip.AddrPort, code uint8, reqLen int) []Packet {
	text := ErrorText[code]
	if HeaderLen+2+len(text) > reqLen {
		text = ""
	}
	return []Packet{{To: to, Data: mustEncode(Error{Code: code, Text: text})}}
}

// source returns the rate-limit state for ip, creating it if needed. It
// returns nil when the table is full (the datagram is then dropped).
func (r *Relay) source(now time.Time, ip netip.Addr) *source {
	key := sourceKey(ip)
	s := r.sources[key]
	if s == nil {
		if len(r.sources) >= r.cfg.MaxTrackedSources {
			return nil
		}
		s = &source{hellos: make([]time.Time, r.cfg.HellosPerMinute)}
		r.sources[key] = s
	}
	s.lastSeen = now
	return s
}

// sourceKey maps an address to its rate-limit key: the IPv4 address, or the
// /64 prefix for IPv6 (one IPv6 host usually owns a whole /64).
func sourceKey(ip netip.Addr) netip.Addr {
	ip = ip.Unmap()
	if ip.Is6() {
		if p, err := ip.Prefix(64); err == nil {
			return p.Addr()
		}
	}
	return ip
}

// allowMsg counts one datagram in a fixed one-second window.
func (s *source) allowMsg(now time.Time, perSecond int) bool {
	if now.Sub(s.winStart) >= time.Second || now.Before(s.winStart) {
		s.winStart = now
		s.winCount = 0
	}
	s.winCount++
	return s.winCount <= perSecond
}

// allowHello admits a HELLO if fewer than len(s.hellos) were admitted in the
// last minute, and records it.
func (s *source) allowHello(now time.Time) bool {
	oldest := s.hellos[s.helloNext]
	if !oldest.IsZero() && now.Sub(oldest) < time.Minute {
		return false
	}
	s.hellos[s.helloNext] = now
	s.helloNext = (s.helloNext + 1) % len(s.hellos)
	return true
}

// validName: 1..24 bytes of valid UTF-8.
func validName(name string) bool {
	return len(name) >= 1 && len(name) <= MaxNameLen && utf8.ValidString(name)
}

var errNoRoomCode = errors.New("could not find a free room code")

// newRoomCode draws a random unused room code from RoomAlphabet.
func (r *Relay) newRoomCode() (RoomCode, error) {
	var code RoomCode
	for attempt := 0; attempt < 16; attempt++ {
		var raw [RoomCodeLen]byte
		if _, err := io.ReadFull(r.cfg.Rand, raw[:]); err != nil {
			return code, err
		}
		for i, b := range raw {
			code[i] = RoomAlphabet[b&31] // 32 symbols: no modulo bias
		}
		if _, taken := r.rooms[code]; !taken {
			return code, nil
		}
	}
	return code, errNoRoomCode
}

// anon returns a short salted hash of the address for logs, so log lines can
// be correlated without recording who played.
func (r *Relay) anon(a netip.AddrPort) string {
	h := sha256.New()
	h.Write(r.salt[:])
	h.Write([]byte(a.String()))
	return hex.EncodeToString(h.Sum(nil)[:4])
}

// mustEncode encodes a message whose fields the relay itself controls.
func mustEncode(m Message) []byte {
	b, err := Encode(m)
	if err != nil {
		panic("relay: encode " + err.Error())
	}
	return b
}
