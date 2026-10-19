// Command relayprobe is a small test client for the netplay relay. It speaks
// protocol version 1 and is meant for checking a deployed server from a PC.
//
// Modes:
//
//	relayprobe -server relay.example.net:27777                 # pair (default): two clients in one process
//	relayprobe -server HOST:27777 -mode create                 # create a room, print its code, wait for a peer
//	relayprobe -server HOST:27777 -mode join -room ABCDEF      # join that room from another machine
//
// In every mode it pings once a second and prints the round-trip time from
// PING/PONG, sends -n DATA packets once a peer is present, and prints every
// message that arrives. Pair mode checks the whole flow (create, join, PEER,
// DATA both ways, PING/PONG, BYE, peer-left, ERROR 7 for a dead token) and
// exits non-zero on any failure.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"defjam/netplay/relay"
)

func main() {
	server := flag.String("server", fmt.Sprintf("127.0.0.1:%d", relay.DefaultPort), "relay address host:port")
	mode := flag.String("mode", "pair", "pair | create | join")
	roomFlag := flag.String("room", "", "room code to join (-mode join)")
	name := flag.String("name", "probe", "player name sent in HELLO (1..24 bytes)")
	buildHex := flag.String("build", "5052-4f42-4500-0000", "build fingerprint, 8 bytes as hex (dashes ignored)")
	n := flag.Int("n", 10, "number of DATA packets to send once a peer is present")
	interval := flag.Duration("interval", 100*time.Millisecond, "time between DATA packets")
	wait := flag.Duration("wait", 5*time.Minute, "create/join: how long to wait for a peer")
	timeout := flag.Duration("timeout", 5*time.Second, "time to wait for WELCOME and other replies")
	flag.Parse()

	var build relay.Build
	raw, err := hex.DecodeString(strings.ReplaceAll(*buildHex, "-", ""))
	if err != nil || len(raw) != relay.BuildLen {
		fatalf("-build must be %d bytes of hex", relay.BuildLen)
	}
	copy(build[:], raw)
	if *n < 0 || *n > 100000 {
		fatalf("-n out of range")
	}

	var ok bool
	switch *mode {
	case "pair":
		ok = runPair(*server, build, *name, *n, *interval, *timeout)
	case "create", "join":
		var code relay.RoomCode
		if *mode == "join" {
			code, err = relay.ParseRoomCode(strings.ToUpper(*roomFlag))
			if err != nil || !code.Valid() {
				fatalf("-room must be a 6-character code from %s", relay.RoomAlphabet)
			}
		}
		ok = runSingle(*server, *mode == "join", code, build, *name, *n, *interval, *wait, *timeout)
	default:
		fatalf("unknown -mode %q", *mode)
	}
	if !ok {
		os.Exit(1)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "relayprobe: "+format+"\n", args...)
	os.Exit(2)
}

// ---- one protocol client ----

type event struct {
	msg relay.Message
	at  time.Time
}

type client struct {
	label   string
	conn    *net.UDPConn
	events  chan event
	pending []event // events read while waiting for something else

	token relay.Token
	slot  uint8
	room  relay.RoomCode

	nonce uint32
	pings map[uint32]time.Time
	rtts  []time.Duration
}

func dial(server, label string) (*client, error) {
	raddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return nil, err
	}
	c := &client{label: label, conn: conn, events: make(chan event, 1024), pings: map[uint32]time.Time{}}
	go c.readLoop()
	return c, nil
}

func (c *client) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				close(c.events)
				return
			}
			continue // e.g. ICMP port unreachable while the server is down
		}
		data := append([]byte(nil), buf[:n]...)
		msg, err := relay.DecodeServer(data)
		if err != nil {
			c.logf("malformed datagram from server (%d bytes)", n)
			continue
		}
		c.events <- event{msg: msg, at: time.Now()}
	}
}

func (c *client) logf(format string, args ...any) {
	fmt.Printf("%s [%s] %s\n", time.Now().Format("15:04:05.000"), c.label, fmt.Sprintf(format, args...))
}

func (c *client) send(m relay.Message) {
	b, err := relay.Encode(m)
	if err != nil {
		c.logf("encode: %v", err)
		return
	}
	_, _ = c.conn.Write(b)
}

func (c *client) ping() {
	c.nonce++
	c.pings[c.nonce] = time.Now()
	c.send(relay.Ping{Token: c.token, Nonce: c.nonce})
}

// next returns the next event, or false after d.
func (c *client) next(d time.Duration) (event, bool) {
	if len(c.pending) > 0 {
		e := c.pending[0]
		c.pending = c.pending[1:]
		return e, true
	}
	select {
	case e, ok := <-c.events:
		return e, ok
	case <-time.After(d):
		return event{}, false
	}
}

// hello sends HELLO every 500 ms until WELCOME or ERROR arrives.
func (c *client) hello(h relay.Hello, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.send(h)
		retry := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(retry) {
			e, ok := c.next(time.Until(retry))
			if !ok {
				break
			}
			switch m := e.msg.(type) {
			case relay.Welcome:
				c.token, c.slot, c.room = m.Token, m.Slot, m.Room
				c.logf("WELCOME slot=%d room=%s", m.Slot, m.Room)
				return nil
			case relay.Error:
				return fmt.Errorf("ERROR %d %q", m.Code, m.Text)
			default:
				c.pending = append(c.pending, e)
			}
		}
	}
	return errors.New("no WELCOME (server unreachable, or UDP blocked by a firewall?)")
}

// handle prints and accounts for one event. It returns the message for
// callers that need to react to it.
func (c *client) handle(e event) relay.Message {
	switch m := e.msg.(type) {
	case relay.Pong:
		if t, ok := c.pings[m.Nonce]; ok {
			rtt := e.at.Sub(t)
			delete(c.pings, m.Nonce)
			c.rtts = append(c.rtts, rtt)
			c.logf("PONG nonce=%d rtt=%s", m.Nonce, rtt.Round(10*time.Microsecond))
		} else {
			c.logf("PONG nonce=%d (unknown nonce)", m.Nonce)
		}
	case relay.Peer:
		if m.Name == "" {
			c.logf("PEER slot=%d left", m.Slot)
		} else {
			c.logf("PEER slot=%d name=%q", m.Slot, m.Name)
		}
	case relay.ServerData:
		extra := ""
		if seq, sent, ok := parseProbePayload(m.Payload); ok {
			extra = fmt.Sprintf(" seq=%d", seq)
			if !sent.IsZero() {
				// Only meaningful when sender and receiver share a clock (pair mode).
				extra += fmt.Sprintf(" via-relay=%s", e.at.Sub(sent).Round(10*time.Microsecond))
			}
		}
		c.logf("DATA from=%d %d bytes%s", m.From, len(m.Payload), extra)
	case relay.Error:
		c.logf("ERROR %d %q", m.Code, m.Text)
	case relay.Welcome:
		c.logf("WELCOME slot=%d room=%s (repeat)", m.Slot, m.Room)
	}
	return e.msg
}

func (c *client) rttSummary() string {
	if len(c.rtts) == 0 {
		return "no PONG received"
	}
	minR, maxR, sum := c.rtts[0], c.rtts[0], time.Duration(0)
	for _, r := range c.rtts {
		minR, maxR, sum = min(minR, r), max(maxR, r), sum+r
	}
	avg := sum / time.Duration(len(c.rtts))
	return fmt.Sprintf("rtt min/avg/max = %s/%s/%s over %d pongs (%d pings unanswered)",
		minR.Round(10*time.Microsecond), avg.Round(10*time.Microsecond), maxR.Round(10*time.Microsecond),
		len(c.rtts), len(c.pings))
}

// Probe payload: "DJPROBE" + seq u32 + send time (unix ns, i64), little-endian.
const probeTag = "DJPROBE"

func probePayload(seq uint32) []byte {
	b := append([]byte(probeTag), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(b[len(probeTag):], seq)
	binary.LittleEndian.PutUint64(b[len(probeTag)+4:], uint64(time.Now().UnixNano()))
	return b
}

func parseProbePayload(p []byte) (uint32, time.Time, bool) {
	if len(p) != len(probeTag)+12 || string(p[:len(probeTag)]) != probeTag {
		return 0, time.Time{}, false
	}
	seq := binary.LittleEndian.Uint32(p[len(probeTag):])
	ns := int64(binary.LittleEndian.Uint64(p[len(probeTag)+4:]))
	return seq, time.Unix(0, ns), true
}

// ---- create / join: one client, a real peer elsewhere ----

func runSingle(server string, join bool, code relay.RoomCode, build relay.Build, name string,
	n int, interval, wait, timeout time.Duration) bool {
	c, err := dial(server, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "relayprobe:", err)
		return false
	}
	defer c.conn.Close()

	h := relay.Hello{Mode: relay.ModeCreate, Build: build, Name: name}
	if join {
		h.Mode, h.Room = relay.ModeJoin, code
	}
	if err := c.hello(h, timeout); err != nil {
		c.logf("HELLO failed: %v", err)
		return false
	}
	if !join {
		c.logf("room code: %s (join with: relayprobe -server %s -mode join -room %s)", c.room, server, c.room)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	pingT := time.NewTicker(time.Second)
	defer pingT.Stop()
	c.ping()

	var dataC <-chan time.Time
	sent, got := 0, 0
	peerDeadline := time.Now().Add(wait)
	var finishAt time.Time
	haveFinish := false
	housekeeping := time.NewTicker(100 * time.Millisecond)
	defer housekeeping.Stop()
	startData := func() {
		if dataC == nil && n > 0 {
			t := time.NewTicker(interval)
			dataC = t.C
		} else if n == 0 && !haveFinish {
			finishAt, haveFinish = time.Now().Add(3*time.Second), true
		}
	}
	for _, e := range c.pending {
		if p, ok := c.handle(e).(relay.Peer); ok && p.Name != "" {
			startData()
		}
	}
	c.pending = nil

	result := true
loop:
	for {
		select {
		case e, ok := <-c.events:
			if !ok {
				break loop
			}
			switch m := c.handle(e).(type) {
			case relay.Peer:
				if m.Name != "" {
					startData()
				} else {
					break loop
				}
			case relay.ServerData:
				got++
			case relay.Error:
				if m.Code == relay.ErrUnknownToken {
					result = false
					break loop
				}
			}
		case <-pingT.C:
			c.ping()
		case <-dataC:
			if sent < n {
				sent++
				c.send(relay.ClientData{Token: c.token, Payload: probePayload(uint32(sent))})
			}
			if sent >= n && !haveFinish {
				dataC = nil
				finishAt, haveFinish = time.Now().Add(3*time.Second), true
			}
		case <-housekeeping.C:
			if dataC == nil && !haveFinish && time.Now().After(peerDeadline) {
				c.logf("no peer within %s", wait)
				result = false
				break loop
			}
			if haveFinish && time.Now().After(finishAt) {
				break loop
			}
		case <-sig:
			c.logf("interrupted")
			break loop
		}
	}
	c.send(relay.Bye{Token: c.token})
	c.logf("BYE sent; DATA sent=%d received=%d; %s", sent, got, c.rttSummary())
	return result
}

// ---- pair: two clients in this process, full self-check ----

func runPair(server string, build relay.Build, name string, n int, interval, timeout time.Duration) bool {
	failures := 0
	check := func(cond bool, format string, args ...any) {
		status := "ok  "
		if !cond {
			status = "FAIL"
			failures++
		}
		fmt.Printf("%s %s\n", status, fmt.Sprintf(format, args...))
	}

	a, err := dial(server, "A")
	if err != nil {
		fmt.Fprintln(os.Stderr, "relayprobe:", err)
		return false
	}
	defer a.conn.Close()
	b, err := dial(server, "B")
	if err != nil {
		fmt.Fprintln(os.Stderr, "relayprobe:", err)
		return false
	}
	defer b.conn.Close()

	if err := a.hello(relay.Hello{Mode: relay.ModeCreate, Build: build, Name: name + "-A"}, timeout); err != nil {
		check(false, "create room: %v", err)
		return false
	}
	check(a.slot == relay.SlotHost && a.room.Valid(), "create: slot=%d room=%s", a.slot, a.room)

	if err := b.hello(relay.Hello{Mode: relay.ModeJoin, Room: a.room, Build: build, Name: name + "-B"}, timeout); err != nil {
		check(false, "join room: %v", err)
		a.send(relay.Bye{Token: a.token})
		return false
	}
	check(b.slot == relay.SlotGuest && b.room == a.room, "join: slot=%d room=%s", b.slot, b.room)

	// Both sides must hear about each other.
	waitPeer := func(c *client, wantSlot uint8, wantName string) bool {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			e, ok := c.next(time.Until(deadline))
			if !ok {
				break
			}
			if p, isPeer := c.handle(e).(relay.Peer); isPeer {
				return p.Slot == wantSlot && p.Name == wantName
			}
		}
		return false
	}
	check(waitPeer(b, relay.SlotHost, name+"-A"), "B got PEER about A")
	check(waitPeer(a, relay.SlotGuest, name+"-B"), "A got PEER about B")

	// DATA both ways plus pings, for n*interval and a grace period.
	gotA, gotB := 0, 0
	badFrom := 0
	pingT := time.NewTicker(250 * time.Millisecond)
	defer pingT.Stop()
	dataT := time.NewTicker(interval)
	defer dataT.Stop()
	a.ping()
	b.ping()
	sent := 0
	aEv, bEv := a.events, b.events
	end := time.Now().Add(time.Duration(n)*interval + 2*time.Second)
	for time.Now().Before(end) && !(sent >= n && gotA >= n && gotB >= n && len(a.rtts) >= 3 && len(b.rtts) >= 3) {
		select {
		case e, ok := <-aEv:
			if !ok {
				aEv = nil // socket closed; stop selecting on it
				continue
			}
			if d, isData := a.handle(e).(relay.ServerData); isData {
				gotA++
				if d.From != relay.SlotGuest {
					badFrom++
				}
			}
		case e, ok := <-bEv:
			if !ok {
				bEv = nil
				continue
			}
			if d, isData := b.handle(e).(relay.ServerData); isData {
				gotB++
				if d.From != relay.SlotHost {
					badFrom++
				}
			}
		case <-dataT.C:
			if sent < n {
				sent++
				a.send(relay.ClientData{Token: a.token, Payload: probePayload(uint32(sent))})
				b.send(relay.ClientData{Token: b.token, Payload: probePayload(uint32(sent))})
			}
		case <-pingT.C:
			a.ping()
			b.ping()
		case <-time.After(100 * time.Millisecond):
		}
	}
	check(gotB == n, "A->B DATA: %d/%d delivered", gotB, n)
	check(gotA == n, "B->A DATA: %d/%d delivered", gotA, n)
	check(badFrom == 0, "DATA 'from' slots correct (%d wrong)", badFrom)
	check(len(a.rtts) > 0, "A %s", a.rttSummary())
	check(len(b.rtts) > 0, "B %s", b.rttSummary())

	// B leaves; A must be told with an empty-name PEER.
	b.send(relay.Bye{Token: b.token})
	check(waitPeer(a, relay.SlotGuest, ""), "A got peer-left after B's BYE")

	// B's token is dead now: a PING must be answered with ERROR 7.
	b.send(relay.Ping{Token: b.token, Nonce: 0xDEAD})
	got7 := false
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && !got7 {
		e, ok := b.next(time.Until(deadline))
		if !ok {
			break
		}
		if m, isErr := b.handle(e).(relay.Error); isErr && m.Code == relay.ErrUnknownToken {
			got7 = true
		}
	}
	check(got7, "dead token answered with ERROR 7")

	a.send(relay.Bye{Token: a.token})
	if failures > 0 {
		fmt.Printf("FAIL: %d check(s) failed against %s\n", failures, server)
		return false
	}
	fmt.Printf("PASS: relay at %s works (room %s)\n", server, a.room)
	return true
}
