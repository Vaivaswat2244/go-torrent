package peers

import (
	"fmt"
	"io"
)

// Handshake represents a BitTorrent handshake message
type Handshake struct {
	Pstr     string   // Protocol string (always "BitTorrent protocol")
	Reserved [8]byte  // Reserved bytes; advertise supported extensions
	InfoHash [20]byte // Info hash of the torrent
	PeerID   [20]byte // Our peer ID
}

// ExtensionBit marks BEP 10 extended messaging support, in the 6th reserved byte.
const ExtensionBit = 0x10

// Serialize converts handshake to bytes for sending over network
func (h *Handshake) Serialize() []byte {
	buf := make([]byte, 68) // Total handshake size

	// Byte 0: Length of protocol string (19)
	buf[0] = byte(len(h.Pstr))

	// Bytes 1-19: Protocol string
	copy(buf[1:20], h.Pstr)

	// THE EXTENSION BIT (BEP 10)
	// Bytes 20-27: Reserved bytes. We flip the 43rd bit to 1.
	// This lives in byte 25 (the 6th reserved byte).
	// It tells the peer: "Hey, I support Extended Messaging!"
	buf[25] = 0x10

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

	var reserved [8]byte
	var infoHash [20]byte
	var peerID [20]byte

	// The reserved bytes used to be discarded, so there was no way to tell
	// whether a peer supported extensions.
	copy(reserved[:], buf[20:28])
	copy(infoHash[:], buf[28:48])
	copy(peerID[:], buf[48:68])

	return &Handshake{
		Pstr:     pstr,
		Reserved: reserved,
		InfoHash: infoHash,
		PeerID:   peerID,
	}, nil
}

// SupportsExtensions reports whether the peer set the BEP 10 extension bit.
func (h *Handshake) SupportsExtensions() bool {
	return h.Reserved[5]&ExtensionBit != 0
}

// New creates a new handshake
func New(infoHash, peerID [20]byte) *Handshake {
	return &Handshake{
		Pstr:     "BitTorrent protocol",
		InfoHash: infoHash,
		PeerID:   peerID,
	}
}
