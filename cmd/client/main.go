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

		// Phase 2: Try downloading from multiple peers
		fmt.Println("\n=== Phase 2: Attempting to download piece 0 ===")
		success := false
		maxAttempts := 10 // Try up to 10 peers
		if len(peers) < maxAttempts {
			maxAttempts = len(peers)
		}

		for i := 0; i < maxAttempts && !success; i++ {
			fmt.Printf("\n[%d/%d] Trying peer %s...\n", i+1, maxAttempts, peers[i].String())
			err = testDownloadPiece(tf, peers[i], peerID)
			if err != nil {
				log.Printf("  ❌ Failed: %v\n", err)
			} else {
				success = true
				fmt.Println("\n🎉 SUCCESS! Downloaded and verified piece 0!")
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

func testDownloadPiece(tf *torrentfile.TorrentFile, peer torrentfile.Peer, peerID [20]byte) error {
	// Connect to peer with short timeout
	conn, err := net.DialTimeout("tcp", peer.String(), 3*time.Second)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer conn.Close()
	fmt.Println("  ✅ Connected!")

	// Complete handshake
	fmt.Println("  Handshaking...")
	client, err := peersPkg.CompleteHandshake(conn, tf.InfoHash, peerID)
	if err != nil {
		return fmt.Errorf("handshake failed: %w", err)
	}
	fmt.Println("  ✅ Handshake complete!")

	// Read bitfield
	fmt.Println("  Reading bitfield...")
	msg, err := client.ReadMessage()
	if err != nil {
		return fmt.Errorf("failed to read bitfield: %w", err)
	}

	if msg == nil || msg.ID != peersPkg.MsgBitfield {
		return fmt.Errorf("expected bitfield, got %v", msg)
	}
	client.Bitfield = msg.Payload
	fmt.Printf("  ✅ Bitfield received (%d bytes)\n", len(client.Bitfield))

	// Check if peer has piece 0
	bf := p2p.Bitfield(client.Bitfield)
	if !bf.HasPiece(0) {
		return fmt.Errorf("peer doesn't have piece 0")
	}
	fmt.Println("  ✅ Peer has piece 0!")

	// Send interested
	fmt.Println("  Sending interested...")
	err = client.SendInterested()
	if err != nil {
		return fmt.Errorf("failed to send interested: %w", err)
	}

	// Wait for unchoke
	fmt.Println("  Waiting for unchoke...")
	unchokeTimeout := time.After(10 * time.Second)
	for client.Choked {
		select {
		case <-unchokeTimeout:
			return fmt.Errorf("timeout waiting for unchoke")
		default:
			client.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			msg, err := client.ReadMessage()
			if err != nil {
				return fmt.Errorf("failed to read message: %w", err)
			}
			if msg != nil && msg.ID == peersPkg.MsgUnchoke {
				client.Choked = false
				fmt.Println("  ✅ Unchoked!")
			}
		}
	}

	// Download piece 0
	fmt.Println("  Downloading piece 0...")
	work := &p2p.PieceWork{
		Index:  0,
		Hash:   tf.PieceHashes[0],
		Length: tf.PieceLength,
	}

	buf, err := work.Download(client)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	fmt.Printf("  ✅ Downloaded %d bytes!\n", len(buf))

	// Verify integrity
	fmt.Println("  Verifying SHA-1 hash...")
	err = work.CheckIntegrity(buf)
	if err != nil {
		return fmt.Errorf("integrity check failed: %w", err)
	}
	fmt.Println("  ✅ Hash verified!")

	return nil
}
