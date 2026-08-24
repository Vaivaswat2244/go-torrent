package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"log"

	"github.com/Vaivaswat2244/go-torrent/internal/engine"
)

// defaultPort is the TCP port we listen on and advertise to trackers.
const defaultPort = 6881

// generatePeerID builds a BEP 20 style peer ID: "-GT0001-" plus 12 random bytes.
// This was previously the fixed string "-GO0001-123456789012", so every copy of
// the client presented an identical identity to trackers and peers.
func generatePeerID() ([20]byte, error) {
	var id [20]byte
	copy(id[:], "-GT0001-")
	if _, err := rand.Read(id[8:]); err != nil {
		return id, fmt.Errorf("failed to generate peer ID: %w", err)
	}
	// Keep the random tail printable so it survives logs and tracker UIs.
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for i := 8; i < 20; i++ {
		id[i] = alphabet[int(id[i])%len(alphabet)]
	}
	return id, nil
}

func main() {
	outputDir := flag.String("output", ".", "Output directory")
	port := flag.Uint("port", defaultPort, "TCP port to listen on for incoming peers")
	maxPeers := flag.Int("max-peers", engine.DefaultMaxPeers, "Maximum simultaneous peer connections")
	seedRatio := flag.Float64("seed-ratio", 0, "Stop seeding at this upload/download ratio (0 = unlimited)")
	seedTime := flag.Duration("seed-time", 0, "Stop seeding after this long, e.g. 2h (0 = unlimited)")
	flag.Parse()

	if *port > 65535 {
		log.Fatalf("invalid port %d", *port)
	}

	peerID, err := generatePeerID()
	if err != nil {
		log.Fatal(err)
	}

	runTUI(peerID, *outputDir, uint16(*port), engine.Limits{
		MaxPeers:  *maxPeers,
		SeedRatio: *seedRatio,
		SeedTime:  *seedTime,
	})
}
