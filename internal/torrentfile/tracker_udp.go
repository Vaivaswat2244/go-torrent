package torrentfile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"time"
)

const (
	connectAction  = 0
	announceAction = 1

	protocolID = 0x41727101980 // Magic constant for UDP tracker
)

// RequestPeersUDP contacts a UDP tracker and returns peers
func (tf *TorrentFile) RequestPeersUDP(trackerURL string, peerID [20]byte, port uint16) ([]Peer, error) {
	// Parse tracker URL
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid tracker URL: %w", err)
	}

	// Resolve UDP address
	udpAddr, err := net.ResolveUDPAddr("udp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve tracker address: %w", err)
	}

	// Create UDP connection
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to tracker: %w", err)
	}
	defer conn.Close()

	// Set timeout
	conn.SetDeadline(time.Now().Add(15 * time.Second))

	// Step 1: Send connect request and get connection ID
	connectionID, err := udpConnect(conn)
	if err != nil {
		return nil, fmt.Errorf("connect request failed: %w", err)
	}

	// Step 2: Send announce request and get peers
	peers, err := udpAnnounce(conn, connectionID, tf.InfoHash, peerID, port, tf.Length)
	if err != nil {
		return nil, fmt.Errorf("announce request failed: %w", err)
	}

	return peers, nil
}

// udpConnect sends a connect request and returns the connection ID
func udpConnect(conn *net.UDPConn) (uint64, error) {
	// Build connect request
	buf := new(bytes.Buffer)

	// Protocol ID (magic constant)
	binary.Write(buf, binary.BigEndian, uint64(protocolID))

	// Action (0 = connect)
	binary.Write(buf, binary.BigEndian, uint32(connectAction))

	// Transaction ID (random)
	transactionID := rand.Uint32()
	binary.Write(buf, binary.BigEndian, transactionID)

	// Send request
	_, err := conn.Write(buf.Bytes())
	if err != nil {
		return 0, err
	}

	// Read response (16 bytes)
	resp := make([]byte, 16)
	n, err := conn.Read(resp)
	if err != nil {
		return 0, err
	}
	if n != 16 {
		return 0, fmt.Errorf("invalid connect response size: %d", n)
	}

	// Parse response
	respBuf := bytes.NewReader(resp)

	var action uint32
	binary.Read(respBuf, binary.BigEndian, &action)
	if action != connectAction {
		return 0, fmt.Errorf("invalid action in response: %d", action)
	}

	var respTransactionID uint32
	binary.Read(respBuf, binary.BigEndian, &respTransactionID)
	if respTransactionID != transactionID {
		return 0, fmt.Errorf("transaction ID mismatch")
	}

	var connectionID uint64
	binary.Read(respBuf, binary.BigEndian, &connectionID)

	return connectionID, nil
}

// udpAnnounce sends an announce request and returns the peer list
func udpAnnounce(conn *net.UDPConn, connectionID uint64, infoHash [20]byte, peerID [20]byte, port uint16, length int) ([]Peer, error) {
	// Build announce request
	buf := new(bytes.Buffer)

	// Connection ID
	binary.Write(buf, binary.BigEndian, connectionID)

	// Action (1 = announce)
	binary.Write(buf, binary.BigEndian, uint32(announceAction))

	// Transaction ID
	transactionID := rand.Uint32()
	binary.Write(buf, binary.BigEndian, transactionID)

	// Info hash
	buf.Write(infoHash[:])

	// Peer ID
	buf.Write(peerID[:])

	// Downloaded (0 for now)
	binary.Write(buf, binary.BigEndian, uint64(0))

	// Left (total file size)
	binary.Write(buf, binary.BigEndian, uint64(length))

	// Uploaded (0 for now)
	binary.Write(buf, binary.BigEndian, uint64(0))

	// Event (0 = none, 2 = started)
	binary.Write(buf, binary.BigEndian, uint32(2))

	// IP address (0 = default)
	binary.Write(buf, binary.BigEndian, uint32(0))

	// Key (random)
	binary.Write(buf, binary.BigEndian, rand.Uint32())

	// Num want (-1 = default)
	binary.Write(buf, binary.BigEndian, int32(-1))

	// Port
	binary.Write(buf, binary.BigEndian, port)

	// Send request
	_, err := conn.Write(buf.Bytes())
	if err != nil {
		return nil, err
	}

	// Read response (minimum 20 bytes)
	resp := make([]byte, 10000) // Large buffer for peer list
	n, err := conn.Read(resp)
	if err != nil {
		return nil, err
	}
	if n < 20 {
		return nil, fmt.Errorf("response too short: %d bytes", n)
	}

	// Parse response
	respBuf := bytes.NewReader(resp[:n])

	var action uint32
	binary.Read(respBuf, binary.BigEndian, &action)
	if action != announceAction {
		return nil, fmt.Errorf("invalid action in response: %d", action)
	}

	var respTransactionID uint32
	binary.Read(respBuf, binary.BigEndian, &respTransactionID)
	if respTransactionID != transactionID {
		return nil, fmt.Errorf("transaction ID mismatch")
	}

	var interval uint32
	binary.Read(respBuf, binary.BigEndian, &interval)

	var leechers uint32
	binary.Read(respBuf, binary.BigEndian, &leechers)

	var seeders uint32
	binary.Read(respBuf, binary.BigEndian, &seeders)

	// Parse peer list (6 bytes per peer)
	peersData := resp[20:n]
	peers, err := parsePeers(peersData)
	if err != nil {
		return nil, err
	}

	return peers, nil
}
