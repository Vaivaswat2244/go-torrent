package torrentfile

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/anacrolix/torrent/bencode"
)

// Peer represents a single peer
type Peer struct {
	IP   net.IP
	Port uint16
}

// TrackerResponse represents the tracker's response
type bencodeTrackerResp struct {
	Interval int    `bencode:"interval"`
	Peers    string `bencode:"peers"`
}

// RequestPeers contacts the tracker (HTTP or UDP) and returns a list of peers
// It tries the main announce URL first, then falls back to announce-list
func (tf *TorrentFile) RequestPeers(peerID [20]byte, port uint16) ([]Peer, error) {
	// Build list of all trackers to try
	var trackers []string

	// Add primary announce if present
	if tf.Announce != "" {
		trackers = append(trackers, tf.Announce)
	}

	// Add announce-list trackers
	for _, tier := range tf.AnnounceList {
		trackers = append(trackers, tier...)
	}

	if len(trackers) == 0 {
		return nil, fmt.Errorf("no trackers found in torrent file")
	}

	// Try each tracker
	var lastErr error
	for _, tracker := range trackers {
		if tracker == "" {
			continue
		}

		peers, err := tf.tryTracker(tracker, peerID, port)
		if err == nil && len(peers) > 0 {
			return peers, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all trackers failed, last error: %w", lastErr)
	}
	return nil, fmt.Errorf("all trackers failed")
}

// tryTracker attempts to contact a single tracker
func (tf *TorrentFile) tryTracker(trackerURL string, peerID [20]byte, port uint16) ([]Peer, error) {
	// Check if tracker is HTTP or UDP
	if len(trackerURL) >= 6 && trackerURL[:6] == "udp://" {
		return tf.RequestPeersUDP(trackerURL, peerID, port)
	}
	if len(trackerURL) >= 7 && (trackerURL[:7] == "http://" || trackerURL[:8] == "https://") {
		return tf.RequestPeersHTTP(trackerURL, peerID, port)
	}
	return nil, fmt.Errorf("unsupported tracker protocol: %s", trackerURL)
}

// RequestPeersHTTP contacts an HTTP tracker and returns a list of peers
func (tf *TorrentFile) RequestPeersHTTP(trackerURL string, peerID [20]byte, port uint16) ([]Peer, error) {
	// Build tracker URL with query parameters
	u, err := url.Parse(trackerURL)
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

	// Check status code
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tracker returned status %d", resp.StatusCode)
	}

	// Parse bencode response
	var trackerResp bencodeTrackerResp
	err = bencode.NewDecoder(resp.Body).Decode(&trackerResp)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tracker response: %w", err)
	}

	// Parse compact peer list (6 bytes per peer: 4 for IP, 2 for port)
	return parsePeers([]byte(trackerResp.Peers))
}

// parsePeers converts compact peer format to Peer structs
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

// String returns a readable representation of a peer
func (p Peer) String() string {
	return fmt.Sprintf("%s:%d", p.IP.String(), p.Port)
}
