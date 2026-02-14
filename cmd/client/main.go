package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/p2p"
	peersPkg "github.com/Vaivaswat2244/go-torrent/internal/peers"
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

	// Use a random port to avoid getting our own IP back
	// (In production, you'd use your actual listening port)
	port := uint16(6881)

	peers, err := tf.RequestPeers(peerID, port)
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
			if i >= 10 {
				break
			}
			fmt.Printf("  %d. %s\n", i+1, peer.String())
		}

		// Phase 2: Try downloading from multiple peers concurrently
		fmt.Println("\n=== Phase 2: Attempting to download piece 0 ===")
		fmt.Printf("Trying %d peers concurrently...\n", len(peers))

		// Channel to receive successful download
		resultChan := make(chan error, len(peers))
		successChan := make(chan int, 1) // Channel to signal which peer succeeded

		// Launch goroutine for each peer
		for i, peer := range peers {
			go func(index int, p torrentfile.Peer) {
				err := testDownloadPiece(tf, p, peerID, index+1, len(peers))
				if err == nil {
					// Signal success with peer index
					select {
					case successChan <- index + 1:
						resultChan <- nil
					default:
						// Another peer already succeeded
					}
				} else {
					resultChan <- err
				}
			}(i, peer)
		}

		// Wait for first success or all failures
		success := false
		failCount := 0

		for i := 0; i < len(peers); i++ {
			select {
			case peerNum := <-successChan:
				if !success {
					success = true
					fmt.Printf("\n🎉 SUCCESS! Peer %d/%d completed the download!\n", peerNum, len(peers))
					fmt.Println("✅ Piece 0 downloaded and verified!")
					// Don't break - let other goroutines finish
				}
			case err := <-resultChan:
				if err != nil {
					failCount++
				}
			}

			if success && failCount+1 >= len(peers) {
				// Got success and all other peers finished
				break
			}
		}

		if !success {
			log.Println("\n⚠️  Could not connect to any peers")
			log.Println("Common reasons:")
			log.Println("  - Peers behind firewalls/NAT")
			log.Println("  - Peers went offline")
			log.Println("  - Network connectivity issues")
			log.Println("Try running again - you'll get different peers from tracker")
		}
	}

	// Phase 1: Just print info
	// Later phases: Start downloading
	fmt.Printf("\nOutput directory: %s\n", *outputDir)
	fmt.Println("Ready for Phase 3: Multi-peer download!")
}

func testDownloadPiece(tf *torrentfile.TorrentFile, peer torrentfile.Peer, peerID [20]byte, peerNum, totalPeers int) error {
	fmt.Printf("[%d/%d] Trying %s...\n", peerNum, totalPeers, peer.String())

	// Connect to peer with short timeout
	conn, err := net.DialTimeout("tcp", peer.String(), 3*time.Second)
	if err != nil {
		return fmt.Errorf("connection failed")
	}
	defer conn.Close()
	fmt.Printf("[%d/%d] ✅ Connected!\n", peerNum, totalPeers)

	// Complete handshake
	client, err := peersPkg.CompleteHandshake(conn, tf.InfoHash, peerID)
	if err != nil {
		return fmt.Errorf("handshake failed")
	}
	fmt.Printf("[%d/%d] ✅ Handshake complete\n", peerNum, totalPeers)

	// Read bitfield
	msg, err := client.ReadMessage()
	if err != nil {
		return fmt.Errorf("bitfield read failed")
	}

	if msg == nil || msg.ID != peersPkg.MsgBitfield {
		return fmt.Errorf("expected bitfield")
	}
	client.Bitfield = msg.Payload

	// Check if peer has piece 0
	bf := p2p.Bitfield(client.Bitfield)
	if !bf.HasPiece(0) {
		return fmt.Errorf("peer doesn't have piece 0")
	}
	fmt.Printf("[%d/%d] ✅ Peer has piece 0\n", peerNum, totalPeers)

	// Send interested
	err = client.SendInterested()
	if err != nil {
		return fmt.Errorf("send interested failed")
	}

	// Wait for unchoke
	unchokeTimeout := time.After(10 * time.Second)
	for client.Choked {
		select {
		case <-unchokeTimeout:
			return fmt.Errorf("unchoke timeout")
		default:
			client.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			msg, err := client.ReadMessage()
			if err != nil {
				return fmt.Errorf("read message failed")
			}
			if msg != nil && msg.ID == peersPkg.MsgUnchoke {
				client.Choked = false
				fmt.Printf("[%d/%d] ✅ Unchoked!\n", peerNum, totalPeers)
			}
		}
	}

	// Download piece 0
	fmt.Printf("[%d/%d] Downloading...\n", peerNum, totalPeers)
	work := &p2p.PieceWork{
		Index:  0,
		Hash:   tf.PieceHashes[0],
		Length: tf.PieceLength,
	}

	buf, err := work.Download(client)
	if err != nil {
		return fmt.Errorf("download failed")
	}

	// Verify integrity
	err = work.CheckIntegrity(buf)
	if err != nil {
		return fmt.Errorf("integrity check failed")
	}
	fmt.Printf("[%d/%d] ✅ Downloaded & verified %d bytes!\n", peerNum, totalPeers, len(buf))

	return nil
}
