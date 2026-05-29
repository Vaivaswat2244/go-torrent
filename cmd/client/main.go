package main

import (
	"flag"
	"log"
)

func main() {
	outputDir := flag.String("output", ".", "Output directory")
	flag.Parse()

	var peerID [20]byte
	copy(peerID[:], "-GO0001-123456789012")

	if *outputDir == "" {
		log.Fatal("Usage: torrent [-output <dir>]")
	}

	runTUI(peerID, *outputDir)
}
