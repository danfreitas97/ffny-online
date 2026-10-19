// Command relay is the Def Jam: Fight for NY netplay relay server
// (protocol version 1, docs/netplay-protocol.md). It pairs two players into a
// room and forwards their opaque packets. It keeps no state on disk.
//
//	relay -listen :27777 -max-rooms 512 -max-clients 1024 -log-interval 60s
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"defjam/netplay/relay"
)

func main() {
	listen := flag.String("listen", fmt.Sprintf(":%d", relay.DefaultPort), "UDP address to listen on")
	maxRooms := flag.Int("max-rooms", 512, "maximum number of open rooms")
	maxClients := flag.Int("max-clients", 1024, "maximum number of connected clients")
	logInterval := flag.Duration("log-interval", 60*time.Second, "period of the summary log line (0 disables)")
	flag.Parse()

	if *maxRooms < 1 || *maxClients < 2 {
		log.Fatalf("relay: -max-rooms must be >= 1 and -max-clients >= 2")
	}
	li := *logInterval
	if li == 0 {
		li = -1 // ServerConfig: negative disables, zero means default
	}

	addr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		log.Fatalf("relay: bad -listen %q: %v", *listen, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatalf("relay: listen: %v", err)
	}
	// Generous kernel buffers absorb bursts; failures here are harmless.
	_ = conn.SetReadBuffer(1 << 20)
	_ = conn.SetWriteBuffer(1 << 20)

	srv, err := relay.NewServer(relay.ServerConfig{
		Relay:       relay.Config{MaxRooms: *maxRooms, MaxClients: *maxClients},
		LogInterval: li,
	})
	if err != nil {
		log.Fatalf("relay: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("relay: protocol v%d listening on %s (max-rooms=%d max-clients=%d)",
		relay.Version, conn.LocalAddr(), *maxRooms, *maxClients)
	if err := srv.Serve(ctx, conn); err != nil {
		log.Fatalf("relay: %v", err)
	}
	rooms, clients := srv.Snapshot()
	log.Printf("relay: shut down (dropping %d rooms, %d clients)", rooms, clients)
}
