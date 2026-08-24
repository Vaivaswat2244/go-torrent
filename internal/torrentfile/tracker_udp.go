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
	errorAction    = 3

	protocolID = 0x41727101980 // Magic constant for UDP tracker

	// udpMaxResponse bounds a single datagram read. A tracker returning more
	// peers than fit is truncated by the protocol itself, not by us.
	udpMaxResponse = 65535
)

// announceUDP contacts a UDP tracker (BEP 15) and returns peers plus the
// re-announce interval.
func (tf *TorrentFile) announceUDP(trackerURL string, req AnnounceReq) (*AnnounceResp, error) {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid tracker URL: %w", err)
	}

	udpAddr, err := net.ResolveUDPAddr("udp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve tracker address: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to tracker: %w", err)
	}
	defer conn.Close()

	connectionID, err := udpConnect(conn)
	if err != nil {
		return nil, fmt.Errorf("connect request failed: %w", err)
	}

	resp, err := udpAnnounce(conn, connectionID, tf.InfoHash, req)
	if err != nil {
		return nil, fmt.Errorf("announce request failed: %w", err)
	}

	return resp, nil
}

// udpRoundTrip sends a request and waits for a reply, retrying with the BEP 15
// backoff (15 * 2^n seconds, capped here so a dead tracker doesn't stall the
// whole announce cycle). A single lost datagram used to fail the tracker
// outright.
func udpRoundTrip(conn *net.UDPConn, request []byte) ([]byte, error) {
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		timeout := time.Duration(5*(1<<attempt)) * time.Second
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}

		if _, err := conn.Write(request); err != nil {
			lastErr = err
			continue
		}

		buf := make([]byte, udpMaxResponse)
		n, err := conn.Read(buf)
		if err != nil {
			lastErr = err
			continue
		}
		return buf[:n], nil
	}

	return nil, fmt.Errorf("no response after 3 attempts: %w", lastErr)
}

// udpConnect sends a connect request and returns the connection ID
func udpConnect(conn *net.UDPConn) (uint64, error) {
	buf := new(bytes.Buffer)
	transactionID := rand.Uint32()

	if err := binary.Write(buf, binary.BigEndian, uint64(protocolID)); err != nil {
		return 0, err
	}
	if err := binary.Write(buf, binary.BigEndian, uint32(connectAction)); err != nil {
		return 0, err
	}
	if err := binary.Write(buf, binary.BigEndian, transactionID); err != nil {
		return 0, err
	}

	resp, err := udpRoundTrip(conn, buf.Bytes())
	if err != nil {
		return 0, err
	}
	if len(resp) < 16 {
		return 0, fmt.Errorf("invalid connect response size: %d", len(resp))
	}

	action := binary.BigEndian.Uint32(resp[0:4])
	respTransactionID := binary.BigEndian.Uint32(resp[4:8])

	if respTransactionID != transactionID {
		return 0, fmt.Errorf("transaction ID mismatch")
	}
	if action == errorAction {
		return 0, fmt.Errorf("tracker error: %s", string(resp[8:]))
	}
	if action != connectAction {
		return 0, fmt.Errorf("invalid action in response: %d", action)
	}

	return binary.BigEndian.Uint64(resp[8:16]), nil
}

// udpAnnounce sends an announce request and returns the peer list and interval
func udpAnnounce(conn *net.UDPConn, connectionID uint64, infoHash [20]byte, r AnnounceReq) (*AnnounceResp, error) {
	buf := new(bytes.Buffer)
	transactionID := rand.Uint32()

	fields := []interface{}{
		connectionID,
		uint32(announceAction),
		transactionID,
	}
	for _, f := range fields {
		if err := binary.Write(buf, binary.BigEndian, f); err != nil {
			return nil, err
		}
	}

	buf.Write(infoHash[:])
	buf.Write(r.PeerID[:])

	// Real swarm state, rather than the zeros this used to always send.
	tail := []interface{}{
		uint64(r.Downloaded),
		uint64(r.Left),
		uint64(r.Uploaded),
		uint32(r.Event),
		uint32(0),     // IP address (0 = tracker uses source address)
		rand.Uint32(), // key
		int32(-1),     // num_want (-1 = tracker default)
		r.Port,
	}
	for _, f := range tail {
		if err := binary.Write(buf, binary.BigEndian, f); err != nil {
			return nil, err
		}
	}

	resp, err := udpRoundTrip(conn, buf.Bytes())
	if err != nil {
		return nil, err
	}
	if len(resp) < 8 {
		return nil, fmt.Errorf("response too short: %d bytes", len(resp))
	}

	action := binary.BigEndian.Uint32(resp[0:4])
	respTransactionID := binary.BigEndian.Uint32(resp[4:8])

	if respTransactionID != transactionID {
		return nil, fmt.Errorf("transaction ID mismatch")
	}
	if action == errorAction {
		return nil, fmt.Errorf("tracker error: %s", string(resp[8:]))
	}
	if action != announceAction {
		return nil, fmt.Errorf("invalid action in response: %d", action)
	}
	if len(resp) < 20 {
		return nil, fmt.Errorf("announce response too short: %d bytes", len(resp))
	}

	out := &AnnounceResp{
		Interval: time.Duration(binary.BigEndian.Uint32(resp[8:12])) * time.Second,
		Leechers: int(binary.BigEndian.Uint32(resp[12:16])),
		Seeders:  int(binary.BigEndian.Uint32(resp[16:20])),
	}

	// Trailing bytes that aren't a whole peer are ignored rather than failing
	// the whole announce.
	peersData := resp[20:]
	peersData = peersData[:len(peersData)-len(peersData)%6]

	allPeers, err := parsePeers(peersData)
	if err != nil {
		return nil, err
	}
	out.Peers = filterSelfPeer(allPeers, r.Port)

	return out, nil
}
