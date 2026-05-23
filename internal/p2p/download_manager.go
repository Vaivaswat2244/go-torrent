package p2p

import (
	"fmt"
	"net"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/peers"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

type PeerAttempt struct {
	PeerNum    int
	TotalPeers int
	Peer       torrentfile.Peer
	InfoHash   [20]byte
	PeerID     [20]byte
	Work       *PieceWork
}

type PeerResult struct {
	PeerNum int
	Data    []byte
	Err     error
}

func TryDownloadFromPeer(attempt PeerAttempt) PeerResult {
	result := PeerResult{PeerNum: attempt.PeerNum}

	conn, err := net.DialTimeout("tcp", attempt.Peer.String(), 3*time.Second)
	if err != nil {
		result.Err = fmt.Errorf("connection failed")
		return result
	}
	defer conn.Close()

	client, err := peers.CompleteHandshake(conn, attempt.InfoHash, attempt.PeerID)
	if err != nil {
		result.Err = fmt.Errorf("handshake failed")
		return result
	}

	// Read until Bitfield or timeout
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for i := 0; i < 10; i++ {
		msg, err := client.ReadMessage()
		if err != nil || msg == nil {
			break
		}
		if msg.ID == peers.MsgBitfield {
			client.Bitfield = msg.Payload
			break
		}
	}
	conn.SetReadDeadline(time.Time{})

	bf := Bitfield(client.Bitfield)
	if len(client.Bitfield) > 0 && !bf.HasPiece(attempt.Work.Index) {
		result.Err = fmt.Errorf("peer doesn't have piece %d", attempt.Work.Index)
		return result
	}

	if err := client.SendInterested(); err != nil {
		result.Err = fmt.Errorf("send interested failed")
		return result
	}

	// Wait for unchoke
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for client.Choked {
		msg, err := client.ReadMessage()
		if err != nil {
			result.Err = fmt.Errorf("unchoke wait failed")
			return result
		}
		if msg == nil {
			continue
		}
		switch msg.ID {
		case peers.MsgUnchoke:
			client.Choked = false
		case peers.MsgChoke:
			result.Err = fmt.Errorf("got choked")
			return result
		}
	}
	conn.SetReadDeadline(time.Time{})

	buf, err := attempt.Work.Download(client)
	if err != nil {
		result.Err = fmt.Errorf("download failed: %w", err)
		return result
	}

	if err := attempt.Work.CheckIntegrity(buf); err != nil {
		result.Err = fmt.Errorf("integrity check failed")
		return result
	}

	result.Data = buf
	return result
}

func Worker(peer torrentfile.Peer, tf *torrentfile.TorrentFile, peerID [20]byte, ourBitfield Bitfield, workQueue chan *PieceWork, results chan *PieceResult) {
	conn, err := net.DialTimeout("tcp", peer.String(), 3*time.Second)
	if err != nil {
		return
	}
	defer conn.Close()

	client, err := peers.CompleteHandshake(conn, tf.InfoHash, peerID)
	if err != nil {
		return
	}

	// Send our bitfield
	client.SendBitfield(ourBitfield)

	// Read until we get a Bitfield or give up after a few messages
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for i := 0; i < 10; i++ {
		msg, err := client.ReadMessage()
		if err != nil || msg == nil {
			break
		}
		if msg.ID == peers.MsgBitfield {
			client.Bitfield = msg.Payload
			break
		}
	}
	conn.SetReadDeadline(time.Time{})

	// Tell peer we want data
	if err := client.SendInterested(); err != nil {
		return
	}

	// Wait for unchoke
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for client.Choked {
		msg, err := client.ReadMessage()
		if err != nil {
			return
		}
		if msg == nil {
			continue
		}
		switch msg.ID {
		case peers.MsgUnchoke:
			client.Choked = false
		case peers.MsgChoke:
			return
		case peers.MsgHave:
			if len(msg.Payload) == 4 {
				index := int(msg.Payload[0])<<24 | int(msg.Payload[1])<<16 | int(msg.Payload[2])<<8 | int(msg.Payload[3])
				bf := Bitfield(client.Bitfield)
				bf.SetPiece(index)
			}
		}
	}
	conn.SetReadDeadline(time.Time{})

	// Process work queue
	for work := range workQueue {
		bf := Bitfield(client.Bitfield)
		if len(client.Bitfield) > 0 && !bf.HasPiece(work.Index) {
			workQueue <- work
			time.Sleep(1 * time.Millisecond)
			continue
		}

		buf, err := work.Download(client)
		if err != nil {
			workQueue <- work
			return
		}

		if err := work.CheckIntegrity(buf); err != nil {
			workQueue <- work
			return
		}

		results <- &PieceResult{Index: work.Index, Buf: buf}
	}
}

func DownloadPieceConcurrent(
	peers []torrentfile.Peer,
	infoHash [20]byte,
	peerID [20]byte,
	work *PieceWork,
) ([]byte, error) {
	resultChan := make(chan PeerResult, len(peers))

	for i, peer := range peers {
		go func(peerNum int, p torrentfile.Peer) {
			attempt := PeerAttempt{
				PeerNum:    peerNum + 1,
				TotalPeers: len(peers),
				Peer:       p,
				InfoHash:   infoHash,
				PeerID:     peerID,
				Work:       work,
			}
			resultChan <- TryDownloadFromPeer(attempt)
		}(i, peer)
	}

	var lastErr error
	for i := 0; i < len(peers); i++ {
		result := <-resultChan
		if result.Err == nil {
			return result.Data, nil
		}
		lastErr = result.Err
	}

	return nil, fmt.Errorf("all %d peers failed, last error: %w", len(peers), lastErr)
}
