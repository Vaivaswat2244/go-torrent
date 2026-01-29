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

	// Add public trackers as fallback
	publicTrackers := []string{
		"http://tracker.opentrackr.org:1337/announce",
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://open.demonii.com:1337/announce",
		"udp://tracker.openbittorrent.com:6969/announce",
	}
	trackers = append(trackers, publicTrackers...)

	if len(trackers) == 0 {
		return nil, fmt.Errorf("no trackers found in torrent file")
	}

	// Try each tracker (limit attempts to avoid infinite waiting)
	maxTries := 10 // Only try first 10 trackers
	if len(trackers) < maxTries {
		maxTries = len(trackers)
	}

	var lastErr error
	for i := 0; i < maxTries; i++ {
		tracker := trackers[i]
		if tracker == "" {
			continue
		}

		// Truncate long tracker URLs for display
		displayTracker := tracker
		if len(displayTracker) > 60 {
			displayTracker = displayTracker[:57] + "..."
		}
		fmt.Printf("  [%d/%d] Trying %s\n", i+1, maxTries, displayTracker)

		peers, err := tf.tryTracker(tracker, peerID, port)
		if err == nil && len(peers) > 0 {
			fmt.Printf("  ✅ Success! Got %d peers\n", len(peers))
			return peers, nil
		}
		if err != nil {
			// Show shortened error message
			errMsg := err.Error()
			if len(errMsg) > 80 {
				errMsg = errMsg[:77] + "..."
			}
			fmt.Printf("  ❌ Failed: %s\n", errMsg)
			lastErr = err
		} else {
			fmt.Printf("  ⚠️  No peers returned\n")
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all trackers failed, last error: %w", lastErr)
	}
	return nil, fmt.Errorf("all trackers failed")
}

// tryTracker attempts to contact a single tracker
func (tf *TorrentFile) tryTracker(trackerURL string, peerID [20]byte, port uint16) ([]Peer, error) {
	// Check if tracker is HTTP/HTTPS or UDP
	if len(trackerURL) >= 6 && trackerURL[:6] == "udp://" {
		return tf.RequestPeersUDP(trackerURL, peerID, port)
	}
	if len(trackerURL) >= 7 && trackerURL[:7] == "http://" {
		return tf.RequestPeersHTTP(trackerURL, peerID, port)
	}
	if len(trackerURL) >= 8 && trackerURL[:8] == "https://" {
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
	timeout := 5 * time.Second
	if len(trackerURL) >= 8 && trackerURL[:8] == "https://" {
		timeout = 10 * time.Second // HTTPS needs more time for SSL handshake
	}
	client := &http.Client{Timeout: timeout}

	// Create request with proper headers
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "go-torrent/0.1")

	resp, err := client.Do(req)
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
	allPeers, err := parsePeers([]byte(trackerResp.Peers))
	if err != nil {
		return nil, err
	}

	// Filter out our own IP/port
	peers := filterSelfPeer(allPeers, port)

	return peers, nil
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

// filterSelfPeer removes our own IP from the peer list
func filterSelfPeer(peers []Peer, ourPort uint16) []Peer {
	var filtered []Peer

	// Get our public IP addresses
	ourIPs := getOurIPs()

	for _, peer := range peers {
		isSelf := false

		// Check if this peer matches our IP and port
		for _, ourIP := range ourIPs {
			if peer.IP.Equal(ourIP) && peer.Port == ourPort {
				isSelf = true
				break
			}
		}

		if !isSelf {
			filtered = append(filtered, peer)
		}
	}

	return filtered
}

// getOurIPs returns our local and public IP addresses
func getOurIPs() []net.IP {
	var ips []net.IP

	// Get our local IP
	localIP := getOutboundIP()
	if localIP != nil {
		ips = append(ips, localIP)
	}

	// Note: We can't easily get our public IP without external service
	// The tracker will handle this by not sending us our own IP in most cases
	// But if we do get it, filtering by port should be enough

	return ips
}

// getOutboundIP gets our local IP address
func getOutboundIP() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP
}
