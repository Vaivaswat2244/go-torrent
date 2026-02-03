package p2p

import (
	"crypto/sha1"
	"fmt"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/peers"
)

const MaxBlockSize = 16384 // 16 KB - standard block size
const MaxBacklog = 5       // Max pipelined requests

// PieceWork represents a work item: download this piece
type PieceWork struct {
	Index  int
	Hash   [20]byte
	Length int
}

// PieceResult is the result of downloading a piece
type PieceResult struct {
	Index int
	Buf   []byte
}

// Download downloads a piece from a peer
func (pw *PieceWork) Download(client *peers.Client) ([]byte, error) {
	// Create buffer for the piece
	buf := make([]byte, pw.Length)
	downloaded := 0
	backlog := 0
	requested := 0

	for downloaded < pw.Length {
		// If not choked, send requests
		if !client.Choked {
			for backlog < MaxBacklog && requested < pw.Length {
				blockSize := MaxBlockSize
				if pw.Length-requested < blockSize {
					blockSize = pw.Length - requested
				}

				// Request a block
				err := client.SendRequest(pw.Index, requested, blockSize)
				if err != nil {
					return nil, err
				}

				backlog++
				requested += blockSize
			}
		}

		// Read message from peer
		client.Conn.SetDeadline(time.Now().Add(30 * time.Second))
		msg, err := client.ReadMessage()
		if err != nil {
			return nil, err
		}

		if msg == nil {
			// Keep-alive message
			continue
		}

		switch msg.ID {
		case peers.MsgUnchoke:
			client.Choked = false

		case peers.MsgChoke:
			client.Choked = true

		case peers.MsgHave:
			index, err := peers.ParseHave(msg)
			if err != nil {
				return nil, err
			}
			client.Bitfield = append(client.Bitfield, byte(index))

		case peers.MsgPiece:
			n, err := peers.ParsePiece(pw.Index, buf, msg)
			if err != nil {
				return nil, err
			}
			downloaded += n
			backlog--
		}
	}

	return buf, nil
}

// CheckIntegrity verifies the downloaded piece matches the hash
func (pw *PieceWork) CheckIntegrity(buf []byte) error {
	hash := sha1.Sum(buf)
	if hash != pw.Hash {
		return fmt.Errorf("piece %d failed integrity check", pw.Index)
	}
	return nil
}
