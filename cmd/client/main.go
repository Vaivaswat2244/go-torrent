package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

func main() {
	// Parse command-line flags
	torrentPath := flag.String("torrent", "", "Path to .torrent file")
	outputDir := flag.String("output", ".", "Output directory for downloaded files")
	flag.Parse()

	// Validate inputs
	if *torrentPath == "" {
		fmt.Println("Usage: torrent -torrent <path> [-output <dir>]")
		os.Exit(1)
	}

	// Parse torrent file
	tf, err := torrentfile.Open(*torrentPath)
	if err != nil {
		log.Fatalf("Failed to parse torrent file: %v", err)
	}

	fmt.Printf("Torrent: %s\n", tf.Name)
	fmt.Printf("Info Hash: %x\n", tf.InfoHash)
	fmt.Printf("Piece Length: %d bytes\n", tf.PieceLength)
	fmt.Printf("Total Size: %d bytes\n", tf.Length)
	fmt.Printf("Primary Tracker: %s\n", tf.Announce)

	// Show announce-list if present
	if len(tf.AnnounceList) > 0 {
		fmt.Printf("Backup Trackers: %d tiers\n", len(tf.AnnounceList))
		for i, tier := range tf.AnnounceList {
			fmt.Printf("  Tier %d: %d trackers\n", i+1, len(tier))
			for j, tracker := range tier {
				if j < 3 { // Show first 3 per tier
					fmt.Printf("    - %s\n", tracker)
				}
			}
			if len(tier) > 3 {
				fmt.Printf("    ... and %d more\n", len(tier)-3)
			}
		}
	}

	fmt.Printf("Number of Pieces: %d\n", len(tf.PieceHashes))

	// Generate a peer ID (20 bytes, typically "-GO0001-" + 12 random bytes)
	var peerID [20]byte
	copy(peerID[:], "-GO0001-123456789012")

	// Contact tracker to get peer list
	fmt.Println("\nContacting tracker...")

	// Debug: show what trackers we'll try
	trackerCount := 0
	if tf.Announce != "" {
		trackerCount++
	}
	for _, tier := range tf.AnnounceList {
		trackerCount += len(tier)
	}
	fmt.Printf("Will try %d tracker(s)...\n", trackerCount)

	peers, err := tf.RequestPeers(peerID, 6881)
	if err != nil {
		log.Printf("⚠️  All trackers failed: %v\n", err)
		log.Println("This is normal for old torrents - trackers die frequently")
		log.Println("In Phase 4, we'll add DHT support which solves this problem")
		log.Println("\nFor now, you can:")
		log.Println("1. Try a different torrent with working trackers")
		log.Println("2. Hardcode a known peer IP for testing Phase 2")
		log.Println("3. Continue to Phase 2 with mock data")
	} else if len(peers) == 0 {
		fmt.Println("⚠️  Tracker responded but returned no peers")
		fmt.Println("This means:")
		fmt.Println("  - You might be the only downloader")
		fmt.Println("  - Or all returned peers were filtered (yourself)")
		fmt.Println("  - Try a more popular torrent")
	} else {
		fmt.Printf("✅ Found %d peer(s)!\n", len(peers))
		fmt.Println("\nFirst 10 peers:")
		for i, peer := range peers {
			if i >= len(peers) {
				break
			}
			fmt.Printf("  %d. %s\n", i+1, peer.String())
			i++
		}
	}

	fmt.Printf("\nDownload directory: %s\n", *outputDir)
}
