package p2p

import (
	"fmt"
	"net"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/peers"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// PeerAttempt represents a single peer download attempt
type PeerAttempt struct {
	PeerNum    int
	TotalPeers int
	Peer       torrentfile.Peer
	InfoHash   [20]byte
	PeerID     [20]byte
	Work       *PieceWork
}

// PeerResult is the result from attempting to download from a peer
type PeerResult struct {
	PeerNum int
	Data    []byte
	Err     error
}

// TryDownloadFromPeer attempts to download a piece from a single peer
func TryDownloadFromPeer(attempt PeerAttempt) PeerResult {
	result := PeerResult{
		PeerNum: attempt.PeerNum,
	}

	// Log attempt
	fmt.Printf("[%d/%d] Trying %s...\n", attempt.PeerNum, attempt.TotalPeers, attempt.Peer.String())

	// Connect to peer
	conn, err := net.DialTimeout("tcp", attempt.Peer.String(), 3*time.Second)
	if err != nil {
		result.Err = fmt.Errorf("connection failed")
		return result
	}
	defer conn.Close()

	fmt.Printf("[%d/%d] ✅ Connected!\n", attempt.PeerNum, attempt.TotalPeers)

	// Complete handshake
	client, err := peers.CompleteHandshake(conn, attempt.InfoHash, attempt.PeerID)
	if err != nil {
		result.Err = fmt.Errorf("handshake failed")
		return result
	}
	fmt.Printf("[%d/%d] ✅ Handshake complete\n", attempt.PeerNum, attempt.TotalPeers)

	// Read bitfield
	msg, err := client.ReadMessage()
	if err != nil {
		result.Err = fmt.Errorf("bitfield read failed")
		return result
	}

	if msg == nil || msg.ID != peers.MsgBitfield {
		result.Err = fmt.Errorf("expected bitfield")
		return result
	}
	client.Bitfield = msg.Payload

	// Check if peer has the piece we want
	bf := Bitfield(client.Bitfield)
	if !bf.HasPiece(attempt.Work.Index) {
		result.Err = fmt.Errorf("peer doesn't have piece %d", attempt.Work.Index)
		return result
	}
	fmt.Printf("[%d/%d] ✅ Peer has piece %d\n", attempt.PeerNum, attempt.TotalPeers, attempt.Work.Index)

	// Send interested
	err = client.SendInterested()
	if err != nil {
		result.Err = fmt.Errorf("send interested failed")
		return result
	}

	// Wait for unchoke
	unchokeTimeout := time.After(10 * time.Second)
	for client.Choked {
		select {
		case <-unchokeTimeout:
			result.Err = fmt.Errorf("unchoke timeout")
			return result
		default:
			client.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			msg, err := client.ReadMessage()
			if err != nil {
				result.Err = fmt.Errorf("read message failed")
				return result
			}
			if msg != nil && msg.ID == peers.MsgUnchoke {
				client.Choked = false
				fmt.Printf("[%d/%d] ✅ Unchoked!\n", attempt.PeerNum, attempt.TotalPeers)
			}
		}
	}

	// Download the piece
	fmt.Printf("[%d/%d] Downloading piece %d...\n", attempt.PeerNum, attempt.TotalPeers, attempt.Work.Index)
	buf, err := attempt.Work.Download(client)
	if err != nil {
		result.Err = fmt.Errorf("download failed")
		return result
	}

	// Verify integrity
	err = attempt.Work.CheckIntegrity(buf)
	if err != nil {
		result.Err = fmt.Errorf("integrity check failed")
		return result
	}

	fmt.Printf("[%d/%d] ✅ Downloaded & verified %d bytes!\n",
		attempt.PeerNum, attempt.TotalPeers, len(buf))

	result.Data = buf
	return result
}

func Worker(peer torrentfile.Peer, tf *torrentfile.TorrentFile, peerID [20]byte, ourBitfield Bitfield, workQueue chan *PieceWork, results chan *PieceResult) {
	// 1. Connect and Handshake (Done ONCE per worker)
	conn, err := net.DialTimeout("tcp", peer.String(), 3*time.Second)
	if err != nil {
		return
	} // Peer offline, worker dies silently
	defer conn.Close()

	client, err := peers.CompleteHandshake(conn, tf.InfoHash, peerID)
	if err != nil {
		return
	}

	err = client.SendBitfield(ourBitfield)
	if err != nil {
		return
	}

	msg, err := client.ReadMessage()
	if err != nil || msg == nil || msg.ID != peers.MsgBitfield {
		return
	}
	client.Bitfield = msg.Payload

	// Tell them we want data
	client.SendInterested()

	// 2. Process jobs from the queue
	for work := range workQueue {
		// Does this peer even have the piece we need?
		bf := Bitfield(client.Bitfield)
		if !bf.HasPiece(work.Index) {
			workQueue <- work                 // Put the job back for another worker
			time.Sleep(50 * time.Millisecond) // Don't spinlock the CPU
			continue
		}

		// Try to download
		buf, err := work.Download(client)
		if err == nil {
			err = work.CheckIntegrity(buf)
			if err == nil {
				// Success! Send to results
				results <- &PieceResult{Index: work.Index, Buf: buf}
				continue
			}
		}

		// If we reach here, the download or hash check failed.
		// Put the piece back in the queue and kill this worker
		// (the connection is likely broken or the peer sent bad data).
		workQueue <- work
		return
	}
}

// DownloadPieceConcurrent tries to download a piece from multiple peers concurrently
// Returns the first successful download
func DownloadPieceConcurrent(
	peers []torrentfile.Peer,
	infoHash [20]byte,
	peerID [20]byte,
	work *PieceWork,
) ([]byte, error) {

	fmt.Printf("Trying %d peers concurrently for piece %d...\n", len(peers), work.Index)

	// Create channels for communication
	resultChan := make(chan PeerResult, len(peers))

	// Launch a goroutine for each peer
	for i, peer := range peers {
		// IMPORTANT: We pass peer by value to avoid closure issues
		go func(peerNum int, p torrentfile.Peer) {
			attempt := PeerAttempt{
				PeerNum:    peerNum + 1,
				TotalPeers: len(peers),
				Peer:       p,
				InfoHash:   infoHash,
				PeerID:     peerID,
				Work:       work,
			}

			result := TryDownloadFromPeer(attempt)
			resultChan <- result
		}(i, peer)
	}

	// Wait for results
	// Strategy: Return first success, or all failures
	var lastErr error
	successCount := 0
	for i := 0; i < len(peers); i++ {
		result := <-resultChan

		if result.Err == nil {
			successCount++
			if successCount == 1 {
				// First success! Return the data
				fmt.Printf("\n🎉 SUCCESS! Peer %d/%d completed the download!\n",
					result.PeerNum, len(peers))
				fmt.Println("✅ Piece 0 downloaded and verified!")

				// Return immediately with the data
				// Other goroutines will continue but we ignore their results
				return result.Data, nil
			}
		} else {
			// Track last error for reporting
			lastErr = result.Err
		}
	}

	// All peers failed
	return nil, fmt.Errorf("all %d peers failed, last error: %w", len(peers), lastErr)
}
