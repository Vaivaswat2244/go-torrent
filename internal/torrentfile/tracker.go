package torrentfile

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackpal/bencode-go"
)

type Peer struct {
	IP   net.IP
	Port uint16
}

type bencodeTrackerResp struct {
	Interval int    `bencode:"interval"`
	Peers    string `bencode:"peers"`
}

func (tf *TorrentFile) RequestPeers(peerID [20]byte, port uint16) ([]Peer, error) {
	// Build tracker URL with query parameters
	u, err := url.Parse(tf.Announce)
	if err != nil {
		return nil, fmt.Errorf("invalid tracker URL: %w", err)
	}

	params := url.Values{
		"info_hash":  []string{string(tf.InfoHash[:])},
		"peer_id":    []string{string(peerID[:])},
		"port":       []string{strconv.Itoa(int(port))},
		"uploaded":   []string{"0"},
		"downloaded": []string{"0"},
		"compact":    []string{"1"},
		"left":       []string{strconv.Itoa(tf.Length)},
	}
	u.RawQuery = params.Encode()

	// Make HTTP GET request to tracker
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(u.String())
	if err != nil {
		return nil, fmt.Errorf("tracker request failed: %w", err)
	}
	defer resp.Body.Close()

	// Parse bencode response
	var trackerResp bencodeTrackerResp
	err = bencode.Unmarshal(resp.Body, &trackerResp)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tracker response: %w", err)
	}

	// Parse compact peer list (6 bytes per peer: 4 for IP, 2 for port)
	return parsePeers([]byte(trackerResp.Peers))
}

func parsePeers(peersBin []byte) ([]Peer, error) {
	const peerSize = 6 // 4 bytes IP + 2 bytes port
	numPeers := len(peersBin) / peerSize

	if len(peersBin)%peerSize != 0 {
		return nil, fmt.Errorf("invalid peers length: %d", len(peersBin))
	}

	peers := make([]Peer, numPeers)
	for i := 0; i < numPeers; i++ {
		offset := i * peerSize
		peers[i].IP = net.IP(peersBin[offset : offset+4])
		peers[i].Port = uint16(peersBin[offset+4])<<8 | uint16(peersBin[offset+5])
	}

	return peers, nil
}

func (p Peer) String() string {
	return fmt.Sprintf("%s:%d", p.IP.String(), p.Port)
}
