package peers

import (
	"fmt"
	"net"
	"time"
)

// Client represents a connection to a peer
type Client struct {
	Conn     net.Conn
	Choked   bool
	Bitfield []byte
	peer     net.Addr
	infoHash [20]byte
	peerID   [20]byte
}

// CompleteHandshake performs the BitTorrent handshake with a peer
func CompleteHandshake(conn net.Conn, infoHash, peerID [20]byte) (*Client, error) {
	// Set deadline for handshake
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetDeadline(time.Time{}) // Disable deadline after handshake

	// Send our handshake
	req := New(infoHash, peerID)
	_, err := conn.Write(req.Serialize())
	if err != nil {
		return nil, err
	}

	// Read peer's handshake
	res, err := Read(conn)
	if err != nil {
		return nil, err
	}

	// Verify info hash matches
	if res.InfoHash != infoHash {
		return nil, fmt.Errorf("info hash mismatch")
	}

	return &Client{
		Conn:     conn,
		Choked:   true, // Peers start choked
		Bitfield: []byte{},
		peer:     conn.RemoteAddr(),
		infoHash: infoHash,
		peerID:   peerID,
	}, nil
}

func (c *Client) SendBitfield(bf []byte) error {
	return c.SendMessage(&Message{
		ID:      MsgBitfield,
		Payload: bf,
	})
}

// SendMessage sends a message to the peer
func (c *Client) SendMessage(msg *Message) error {
	_, err := c.Conn.Write(msg.Serialize())
	return err
}

// ReadMessage reads a message from the peer
func (c *Client) ReadMessage() (*Message, error) {
	return ReadMessage(c.Conn)
}

// SendInterested sends an Interested message
func (c *Client) SendInterested() error {
	return c.SendMessage(FormatInterested())
}

// SendNotInterested sends a NotInterested message
func (c *Client) SendNotInterested() error {
	return c.SendMessage(FormatNotInterested())
}

// SendRequest requests a block of data
func (c *Client) SendRequest(index, begin, length int) error {
	return c.SendMessage(FormatRequest(index, begin, length))
}

// SendHave announces we have a piece
func (c *Client) SendHave(index int) error {
	return c.SendMessage(FormatHave(index))
}
