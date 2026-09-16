package peers

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/mse"
)

// Client represents a connection to a peer.
//
// BEP 3 gives a connection four independent pieces of state, two per direction.
// Only PeerChoking (as "Choked") existed before, because the client could only
// download; the other three are what make uploading possible.
type Client struct {
	Conn     net.Conn
	Bitfield []byte

	// Our side of the connection.
	AmChoking    bool // we are refusing to send data to them (starts true)
	AmInterested bool // we want something they have

	// Their side.
	PeerChoking    bool // they are refusing to send data to us (starts true)
	PeerInterested bool // they want something we have

	// PeerID is the remote peer's ID from the handshake. It used to be read
	// and discarded.
	PeerID   [20]byte
	Incoming bool // true if the peer dialed us

	peer     net.Addr
	infoHash [20]byte
	peerID   [20]byte
}

// RemoteAddr returns the peer's address.
func (c *Client) RemoteAddr() net.Addr { return c.peer }

// newClient builds a Client in the BEP 3 initial state: both sides choked and
// uninterested.
func newClient(conn net.Conn, infoHash, ourPeerID, theirPeerID [20]byte, incoming bool) *Client {
	return &Client{
		Conn:        conn,
		Bitfield:    []byte{},
		AmChoking:   true,
		PeerChoking: true,
		PeerID:      theirPeerID,
		Incoming:    incoming,
		peer:        conn.RemoteAddr(),
		infoHash:    infoHash,
		peerID:      ourPeerID,
	}
}

const (
	// handshakeTimeout bounds how long a peer may take to complete a handshake.
	handshakeTimeout = 5 * time.Second

	// mseTimeout bounds the encrypted key exchange, which costs an extra round
	// trip and some Diffie-Hellman arithmetic on the peer's side.
	mseTimeout = 10 * time.Second

	// dialTimeout bounds the TCP connect.
	dialTimeout = 5 * time.Second
)

// CompleteHandshake performs the BitTorrent handshake with a peer
func CompleteHandshake(conn net.Conn, infoHash, peerID [20]byte) (*Client, error) {
	// Set deadline for handshake
	conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetDeadline(time.Time{}) // Disable deadline after handshake

	// Send our handshake
	if _, err := conn.Write(New(infoHash, peerID).Serialize()); err != nil {
		return nil, err
	}

	return FinishHandshake(conn, infoHash, peerID)
}

// FinishHandshake reads and checks the peer's handshake, for when ours has
// already gone out. Under MSE ours travels inside the encrypted handshake's
// initial payload, which saves a round trip, so only the reply is left to read.
func FinishHandshake(conn net.Conn, infoHash, peerID [20]byte) (*Client, error) {
	conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	res, err := Read(conn)
	if err != nil {
		return nil, err
	}

	if res.InfoHash != infoHash {
		return nil, fmt.Errorf("info hash mismatch")
	}

	return newClient(conn, infoHash, peerID, res.PeerID, false), nil
}

// Connect dials a peer and completes the handshake according to policy.
//
// Under PolicyPrefer an encrypted connection is tried first. If the TCP
// connection succeeds but the peer does not speak MSE, it is redialled in the
// clear; a failed dial is not retried, since a second attempt would only fail
// the same way.
//
// When dialling we offer RC4 only. Offering plaintext as well would let the
// peer choose it, and some implementations prefer plaintext when given the
// option, which would leave everything after the handshake readable to the
// filters MSE exists to get past.
func Connect(ctx context.Context, addr string, infoHash, peerID [20]byte, policy mse.Policy) (*Client, error) {
	if policy == mse.PolicyDisable {
		return dialPlain(ctx, addr, infoHash, peerID)
	}

	client, connected, err := dialEncrypted(ctx, addr, infoHash, peerID)
	if err == nil {
		return client, nil
	}
	if policy == mse.PolicyRequire || !connected || ctx.Err() != nil {
		return nil, err
	}

	return dialPlain(ctx, addr, infoHash, peerID)
}

func dialPlain(ctx context.Context, addr string, infoHash, peerID [20]byte) (*Client, error) {
	conn, err := dialTCP(ctx, addr)
	if err != nil {
		return nil, err
	}

	client, err := CompleteHandshake(conn, infoHash, peerID)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return client, nil
}

// dialEncrypted reports whether the TCP connection itself succeeded, so the
// caller can tell "peer unreachable" from "peer does not speak MSE".
func dialEncrypted(ctx context.Context, addr string, infoHash, peerID [20]byte) (*Client, bool, error) {
	conn, err := dialTCP(ctx, addr)
	if err != nil {
		return nil, false, err
	}

	// One deadline covers the key exchange; FinishHandshake sets its own.
	conn.SetDeadline(time.Now().Add(mseTimeout))

	ec, err := mse.Initiate(conn, infoHash, mse.MethodRC4, New(infoHash, peerID).Serialize())
	if err != nil {
		conn.Close()
		return nil, true, err
	}

	client, err := FinishHandshake(ec, infoHash, peerID)
	if err != nil {
		conn.Close()
		return nil, true, err
	}
	return client, true, nil
}

func dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: dialTimeout}
	return dialer.DialContext(ctx, "tcp", addr)
}

// AcceptHandshake completes the handshake on a connection a peer dialed to us.
//
// The order is the mirror of CompleteHandshake: we must read first, because
// until the peer tells us which info hash they want we do not know which
// torrent to answer for. lookup reports whether we serve that torrent.
func AcceptHandshake(
	conn net.Conn,
	ourPeerID [20]byte,
	lookup func(infoHash [20]byte) bool,
) (*Client, error) {
	conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	// Read theirs first to learn the requested torrent.
	req, err := Read(conn)
	if err != nil {
		return nil, fmt.Errorf("reading peer handshake: %w", err)
	}

	if !lookup(req.InfoHash) {
		return nil, fmt.Errorf("peer requested unknown info hash %x", req.InfoHash)
	}

	// Only now do we answer.
	if _, err := conn.Write(New(req.InfoHash, ourPeerID).Serialize()); err != nil {
		return nil, fmt.Errorf("sending handshake: %w", err)
	}

	return newClient(conn, req.InfoHash, ourPeerID, req.PeerID, true), nil
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
	c.AmInterested = true
	return c.SendMessage(FormatInterested())
}

// SendNotInterested sends a NotInterested message
func (c *Client) SendNotInterested() error {
	c.AmInterested = false
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

// SendChoke tells the peer we will not serve them, and records it.
func (c *Client) SendChoke() error {
	c.AmChoking = true
	return c.SendMessage(FormatChoke())
}

// SendUnchoke tells the peer we will serve their requests.
func (c *Client) SendUnchoke() error {
	c.AmChoking = false
	return c.SendMessage(FormatUnchoke())
}

// SendPiece sends one block of piece data to the peer.
func (c *Client) SendPiece(index, begin int, data []byte) error {
	return c.SendMessage(FormatPiece(index, begin, data))
}

// SendCancel withdraws a block request we previously made.
func (c *Client) SendCancel(index, begin, length int) error {
	return c.SendMessage(FormatCancel(index, begin, length))
}

func (c *Client) SendExtendedMessage(payload []byte) error {
	return c.SendMessage(&Message{
		ID:      MsgExtended,
		Payload: payload,
	})
}
