package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/engine"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

func main() {
	torrentPath := flag.String("torrent", "", "Path to .torrent file")
	outputDir := flag.String("output", ".", "Output directory")
	flag.Parse()

	if *torrentPath == "" {
		log.Fatal("Usage: torrent -torrent <path> [-output <dir>]")
	}

	var peerID [20]byte
	copy(peerID[:], "-GO0001-123456789012")

	tf, err := torrentfile.Open(*torrentPath)
	if err != nil {
		log.Fatalf("Failed to parse torrent file: %v", err)
	}

	fmt.Printf("📁 Loaded: %s (%.2f MB)\n", tf.Name, float64(tf.Length)/1024/1024)

	torrent, err := engine.NewTorrent(tf, *outputDir)
	if err != nil {
		log.Fatalf("Failed to initialize engine: %v", err)
	}

	fmt.Println("🚀 Starting Torrent Daemon...")
	torrent.Start(peerID, 6881)

	for {
		stats := torrent.GetStats()

		fmt.Printf("\r[%s] Progress: %05.2f%% | Active Peers: %d   ",
			stats.Status, stats.Progress, stats.PeersActive)

		if stats.Status == engine.StatusSeeding {
			fmt.Printf("\n🎉 Download complete! Now seeding.\n")
			break
		}

		if stats.Status == engine.StatusError {
			fmt.Printf("\n❌ Download failed.\n")
			break
		}

		time.Sleep(1 * time.Second)
	}

	fmt.Println("Daemon shutting down.")
}
