# Netplay relay

A small UDP relay for online play of the Def Jam: Fight for NY port. It pairs two
players into a room and forwards their packets to each other. It never runs the game
and never looks inside the packets: inputs, checksums and seeds are opaque payloads.

The wire protocol is the contract in [`docs/netplay-protocol.md`](../../docs/netplay-protocol.md)
(protocol version 1). This server implements it exactly; the choices it makes where
the specification leaves room are listed under [Spec interpretations](#spec-interpretations).

```
 player A (host, slot 0) --UDP--> relay --UDP--> player B (guest, slot 1)
```

- One static binary, standard library only, no state on disk, no configuration file.
- Default port **UDP 27777**.
- Logs one line per room event (created, joined, peer left, expired, closed) with the
  room code and an anonymised client id (salted hash of the address, new salt every
  start), plus a summary line every minute. Names, tokens and addresses are never logged.

## Layout

| Path | What |
|---|---|
| `protocol.go` | Message encoding/decoding (no state). |
| `relay.go` | Room/client state machine. No sockets; the current time is a parameter, so it is tested with a fake clock. |
| `server.go` | UDP loop: one goroutine reads the socket, one runs the 1 s timer; a mutex guards the state. |
| `cmd/relay` | The server binary. |
| `cmd/relayprobe` | A test client for checking a deployed server from a PC. |
| `deploy/defjam-relay.service` | Hardened systemd unit. |

## Build

Requires Go 1.22 or newer (developed with 1.27). No third-party modules.

Run the tests:

```sh
go test ./...
```

(`go test -race ./...` needs cgo and a C compiler; on a Windows machine without gcc,
run the tests without `-race`.)

Cross-compile the Linux server from any machine:

```sh
# bash / Git Bash / Linux / macOS
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/relay ./cmd/relay
```

```powershell
# PowerShell
$env:GOOS = "linux"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w" -o dist/relay ./cmd/relay
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
```

If `go` is not on `PATH`, call it by its full path, e.g. `& "C:\Program Files\Go\bin\go.exe" build ...`.
`dist/` is git-ignored.

Server flags:

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:27777` | UDP address to listen on (IPv4 and IPv6). |
| `-max-rooms` | `512` | Open rooms; beyond that `HELLO` gets `ERROR 5`. |
| `-max-clients` | `1024` | Connected clients; beyond that `HELLO` gets `ERROR 5`. |
| `-log-interval` | `60s` | Summary line period; `0` disables it. |

## Deploy on a fresh Hetzner Ubuntu 24.04 server

The smallest cloud server (e.g. CX22, 2 vCPU, 4 GB) is far more than enough. Pick a
location close to the players. Below, `SERVER` is the server's public IP.

1. **Update the system** (as root over SSH):

   ```sh
   ssh root@SERVER
   apt update && apt upgrade -y
   ```

2. **Create a system user** for the service (no home, no login shell):

   ```sh
   useradd --system --no-create-home --shell /usr/sbin/nologin defjam-relay
   ```

3. **Copy the binary and the unit** from your PC (Windows 10/11 ships `scp`), in
   `netplay/relay`:

   ```sh
   scp dist/relay deploy/defjam-relay.service root@SERVER:/tmp/
   ```

4. **Install them** on the server:

   ```sh
   install -m 0755 -o root -g root /tmp/relay /usr/local/bin/defjam-relay
   install -m 0644 -o root -g root /tmp/defjam-relay.service /etc/systemd/system/defjam-relay.service
   rm /tmp/relay /tmp/defjam-relay.service
   systemctl daemon-reload
   systemctl enable --now defjam-relay
   systemctl status defjam-relay          # should be "active (running)"
   journalctl -u defjam-relay -f          # live log; Ctrl+C to stop following
   ```

5. **Open the firewall.** Allow SSH *before* enabling ufw, or you lock yourself out:

   ```sh
   ufw allow OpenSSH
   ufw allow 27777/udp
   ufw enable
   ufw status
   ```

   If the server also has a **Hetzner Cloud Firewall** attached (console → Firewalls),
   add an inbound rule there too: UDP, port 27777, any source.

6. **Check it from a PC** with the probe (in `netplay/relay` on the PC):

   ```sh
   go run ./cmd/relayprobe -server SERVER:27777
   ```

   Pair mode (the default) opens two clients in one process and checks create, join,
   PEER, DATA both ways, PING/PONG, BYE, peer-left and ERROR 7 for a dead token. It ends
   with `PASS: relay at ... works` and exit code 0, or lists the failed checks and exits 1.
   `no WELCOME (server unreachable, or UDP blocked by a firewall?)` means nothing came
   back: check steps 4 and 5.

   To test between two machines:

   ```sh
   relayprobe -server SERVER:27777 -mode create            # prints a room code, waits for a peer
   relayprobe -server SERVER:27777 -mode join -room ABC234 # on the other machine
   ```

   Both sides print the PING round-trip time every second and every DATA that
   arrives. Other flags: `-n` (DATA packets to send, default 10), `-interval`,
   `-name`, `-build` (8-byte hex fingerprint), `-wait`, `-timeout`.

Optional check of the sandbox: `systemd-analyze security defjam-relay`.

## Update

Build a new `dist/relay` on the PC, then:

```sh
scp dist/relay root@SERVER:/tmp/relay
ssh root@SERVER 'install -m 0755 /tmp/relay /usr/local/bin/defjam-relay && rm /tmp/relay && systemctl restart defjam-relay'
```

All state is in memory: a restart ends every room. Clients get `ERROR 7` on their next
message and start over with `HELLO`. Restart when nobody is playing, or accept that.
Shutdown on `SIGTERM`/`SIGINT` (what `systemctl stop` sends) is clean.

## Resource needs

- **Memory:** a few MB (a few hundred bytes per client, room and tracked source IP; the
  per-IP table is capped at 100 000 entries, about 40 MB in the worst case, reached only
  under a spoofed flood). The unit caps memory at 128 MB.
- **CPU:** negligible. One core handles far more than 1024 clients at 120 messages/s.
- **Bandwidth:** each client may send up to 400 messages/s; a 60 Hz game with
  redundancy sends about 60-120. At 120 messages/s with ~80-byte payloads, one room
  costs about 30 KB/s in and 30 KB/s out (≈0.5 Mbit/s, ≈200 MB per hour of play).
  512 full rooms would be about 250 Mbit/s. Check the plan's included traffic (20 TB a
  month in Hetzner's EU locations at the time of writing, roughly 90 000 room-hours).
- **Disk:** the binary (≈2.5 MB) and the journal.

## Spec interpretations

The specification is the contract; where it left room, the server takes the simplest
reading below. None of these change the wire format.

1. **Joining uses slot 1 only.** `HELLO` mode 1 joins as slot 1 when slot 1 is free.
   If the guest left, a new guest can join and the host gets a new `PEER`. If the
   *host* left, the remaining guest keeps slot 1 and the room answers `ERROR 2` (room
   full) to new joiners; the guest should `BYE` and create a new room.
2. **Repeated HELLO.** Any `HELLO` from an address and port that already has a token is
   answered with the same `WELCOME`, whatever its contents; if the room is full the
   `PEER` about the other player is sent again too (it may have been lost with the
   `WELCOME`). Such repeats do not count against the HELLO rate limit and refresh the
   idle timer. To change rooms a client sends `BYE` first.
3. **Check order for a new HELLO:** rate limit (`6`), name (`4`), server full (`5`),
   then for a join: room not found (`1`), room full (`2`), build mismatch (`3`).
4. **Names** must be 1..24 bytes of valid UTF-8; anything else is `ERROR 4`. A name
   length byte that disagrees with the datagram size is a malformed packet (dropped).
   Content is not otherwise filtered.
5. **Room codes.** The `room` field of a create is ignored (it need not be zeros). A join
   code is compared byte for byte; the server does not upper-case, so a code outside
   the alphabet is simply `ERROR 1`.
6. **Exact lengths.** `HELLO` is 22 + name bytes, `DATA` 15..1164, `PING` exactly 18,
   `BYE` exactly 14. Extra or missing bytes, `mode` other than 0/1, server-to-client
   types sent to the server, and datagrams over 1200 bytes are dropped silently.
7. **No reply larger than the request.** `ERROR` texts are informational and at most
   14 bytes, so `ERROR 1`-`6` always fit in the smallest `HELLO`. When the text would
   make the reply larger than the request, it is sent **empty**: `ERROR 7` in reply to
   `PING` (18 bytes) or `BYE` (14 bytes) always has an empty text. Clients must act on
   the code only.
8. **Per-IP limits.** "IP" is the IPv4 address, or the /64 prefix for IPv6 (an IPv6 host
   normally owns a whole /64); IPv4-mapped IPv6 counts as IPv4. HELLO: at most 10
   *new* HELLOs (including ones answered with `ERROR 1`-`5`) in any sliding 60 s. The
   400 messages/s limit is a fixed one-second window and counts every datagram,
   malformed ones included.
9. **Error rate limits.** `ERROR 7` is sent at most once per second per source IP (not
   per address and port). `ERROR 6` is likewise sent at most once per second per source
   IP; further limited HELLOs in that second are dropped silently.
10. **BYE** has no reply. A `BYE` with a wrong token is treated like any message with a
    wrong token (`ERROR 7`, rate-limited).
11. **Idle timeout.** Any message with a valid token (and a repeated `HELLO`) resets the
    15 s timer; messages with a bad token and malformed datagrams do not. Timers are
    checked every second, so removal happens 15-16 s after the last message. The
    peer-left `PEER` is sent once and not retried.
12. **10-minute expiry** counts from room creation and applies only to rooms that never
    had a guest. The host is removed without a message (it gets `ERROR 7` on its next
    `PING`). A room whose guest joined and left does not expire while the host stays.
13. **DATA while alone** in a room is dropped silently.
14. **On join** the server sends `WELCOME` to the guest, then `PEER` to the guest, then
    `PEER` to the host. UDP may reorder them.
15. **Source table cap.** The server tracks at most 100 000 source IPs (entries expire
    after about a minute of silence). If the table is full, datagrams from new IPs are
    dropped; this only matters under a spoofed flood.
