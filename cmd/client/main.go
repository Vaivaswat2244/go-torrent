package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/bencode"
	"github.com/Vaivaswat2244/go-torrent/internal/dht"
	"github.com/Vaivaswat2244/go-torrent/internal/engine"
	"github.com/Vaivaswat2244/go-torrent/internal/magnet"
	"github.com/Vaivaswat2244/go-torrent/internal/metadata"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

func main() {
	torrentPath := flag.String("torrent", "", "Path to .torrent file")
	magnetLink := flag.String("magnet", "", "Magnet link string")
	outputDir := flag.String("output", ".", "Output directory")
	flag.Parse()

	if *torrentPath == "" && *magnetLink == "" {
		log.Fatal("Usage: torrent [-torrent <path> | -magnet <link>] [-output <dir>]")
	}
	var peerID [20]byte
	copy(peerID[:], "-GO0001-123456789012")

	var tf *torrentfile.TorrentFile
	var err error

	if *magnetLink != "" {
		fmt.Println("🧲 Parsing Magnet Link...")
		mag, err := magnet.Parse(*magnetLink)
		if err != nil {
			log.Fatalf("Failed to parse magnet link: %v", err)
		}
		fmt.Printf("InfoHash: %x\n", mag.InfoHash)

		// 1. Create a channel for peers
		peerChan := make(chan torrentfile.Peer, 100)
		debugChan := make(chan torrentfile.Peer, 100)

		go func() {
			count := 0
			for p := range debugChan {
				count++
				fmt.Printf("🟢 Peer #%d: %s\n", count, p.String())
				peerChan <- p
			}
		}()

		// 2. Start the DHT Crawler in the background
		go dht.FindPeers(mag.InfoHash, debugChan)
		tempTF := mag.ToTorrentFile()
		for _, trackerURL := range mag.Trackers {
			trackerURL := trackerURL
			go func() {
				peers, err := tempTF.RequestPeersUDP(trackerURL, peerID, 6881)
				if err != nil {
					fmt.Printf("❌ Tracker %s failed: %v\n", trackerURL, err)
					return
				}
				fmt.Printf("✅ Tracker %s returned %d peers\n", trackerURL, len(peers))
				for _, p := range peers {
					select {
					case debugChan <- p:
					default:
					}
				}
			}()
		}

		// 3. Pause and wait for the Metadata Fetcher!
		// THIS replaces the "stub" logic. It downloads the real piece hashes over TCP!
		rawInfo, err := metadata.Fetch(mag.InfoHash, peerID, peerChan)
		if err != nil {
			log.Fatalf("Fatal: Could not fetch metadata from network: %v", err)
		}

		// 4. Reconstruct the TorrentFile using our bencode decoder
		infoDictVal, err := bencode.Decode(rawInfo)
		if err != nil {
			log.Fatalf("Failed to decode downloaded metadata: %v", err)
		}

		infoDict := infoDictVal.(map[string]bencode.Value)

		// 5. Parse the dictionary into our TorrentFile struct
		tf, err = torrentfile.ParseInfoDict(infoDict, mag.InfoHash)
		if err != nil {
			log.Fatalf("Failed to map downloaded metadata: %v", err)
		}

		tf.Name = mag.Name
		fmt.Printf("📁 Metadata Downloaded! Size: %.2f MB\n", float64(tf.Length)/1024/1024)

	} else {
		tf, err = torrentfile.Open(*torrentPath)
		if err != nil {
			log.Fatalf("Failed to parse torrent file: %v", err)
		}
		fmt.Printf("📁 Loaded: %s (%.2f MB)\n", tf.Name, float64(tf.Length)/1024/1024)
	}

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

	fmt.Println("\nDaemon shutting down.")
}
