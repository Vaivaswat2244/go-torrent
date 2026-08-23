package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"log"
)

// listenPort is the port reported to trackers and peers. Nothing listens on it
// yet; this client does not serve pieces.
const listenPort uint16 = 6881

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
	flag.Parse()

	peerID, err := generatePeerID()
	if err != nil {
		log.Fatal(err)
	}

	runTUI(peerID, *outputDir)
}
