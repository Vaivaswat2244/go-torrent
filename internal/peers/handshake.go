package peers

import (
	"fmt"
	"io"
)

// Handshake represents a BitTorrent handshake message
type Handshake struct {
	Pstr     string   // Protocol string (always "BitTorrent protocol")
	InfoHash [20]byte // Info hash of the torrent
	PeerID   [20]byte // Our peer ID
}

// Serialize converts handshake to bytes for sending over network
func (h *Handshake) Serialize() []byte {
	buf := make([]byte, 68) // Total handshake size

	// Byte 0: Length of protocol string (19)
	buf[0] = byte(len(h.Pstr))

	// Bytes 1-19: Protocol string
	copy(buf[1:20], h.Pstr)

	// Bytes 20-27: Reserved bytes (all zeros)
	// Already zero from make()

	// Bytes 28-47: Info hash
	copy(buf[28:48], h.InfoHash[:])

	// Bytes 48-67: Peer ID
	copy(buf[48:68], h.PeerID[:])

	return buf
}

// Read reads a handshake from a connection
func Read(r io.Reader) (*Handshake, error) {
	buf := make([]byte, 68)

	// Read exactly 68 bytes
	_, err := io.ReadFull(r, buf)
	if err != nil {
		return nil, err
	}

	// Parse the handshake
	pstrLen := int(buf[0])
	if pstrLen != 19 {
		return nil, fmt.Errorf("invalid protocol string length: %d", pstrLen)
	}

	pstr := string(buf[1:20])
	if pstr != "BitTorrent protocol" {
		return nil, fmt.Errorf("invalid protocol string: %s", pstr)
	}

	var infoHash [20]byte
	var peerID [20]byte

	copy(infoHash[:], buf[28:48])
	copy(peerID[:], buf[48:68])

	return &Handshake{
		Pstr:     pstr,
		InfoHash: infoHash,
		PeerID:   peerID,
	}, nil
}

// New creates a new handshake
func New(infoHash, peerID [20]byte) *Handshake {
	return &Handshake{
		Pstr:     "BitTorrent protocol",
		InfoHash: infoHash,
		PeerID:   peerID,
	}
}
