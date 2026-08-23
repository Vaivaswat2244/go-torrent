package torrentfile

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/bencode"
)

// Peer represents a single peer
type Peer struct {
	IP   net.IP
	Port uint16
}

// AnnounceEvent is the BEP 3 / BEP 15 event code sent with an announce.
type AnnounceEvent uint32

const (
	EventNone      AnnounceEvent = 0
	EventCompleted AnnounceEvent = 1
	EventStarted   AnnounceEvent = 2
	EventStopped   AnnounceEvent = 3
)

func (e AnnounceEvent) String() string {
	switch e {
	case EventCompleted:
		return "completed"
	case EventStarted:
		return "started"
	case EventStopped:
		return "stopped"
	default:
		return ""
	}
}

// AnnounceReq is the swarm state reported to a tracker. These numbers used to be
// hardcoded (downloaded/uploaded always 0, left always the full torrent length),
// which misreports progress to every tracker, resume included.
type AnnounceReq struct {
	PeerID     [20]byte
	Port       uint16
	Downloaded int64
	Uploaded   int64
	Left       int64
	Event      AnnounceEvent
}

// AnnounceResp is a tracker's reply. Interval matters: without honouring it the
// client announces once and never again, so the peer supply goes stale.
type AnnounceResp struct {
	Peers    []Peer
	Interval time.Duration
	Seeders  int
	Leechers int
}

// SupportedTracker reports whether we can speak this tracker's protocol.
// Torrents commonly list WebTorrent (ws://, wss://) trackers, which are only
// reachable from a browser; announcing to them just produces error noise every
// round.
func SupportedTracker(trackerURL string) bool {
	return strings.HasPrefix(trackerURL, "udp://") ||
		strings.HasPrefix(trackerURL, "http://") ||
		strings.HasPrefix(trackerURL, "https://")
}

// FilterSupportedTrackers drops tracker URLs whose protocol we cannot use.
func FilterSupportedTrackers(urls []string) []string {
	var out []string
	for _, u := range urls {
		if SupportedTracker(u) {
			out = append(out, u)
		}
	}
	return out
}

// maxTrackerResponse bounds how much we read from an HTTP tracker.
const maxTrackerResponse = 1 << 20

// AnnounceTo contacts a single tracker, dispatching on the URL scheme.
//
// The engine previously called RequestPeersUDP directly for every tracker in
// the announce-list, including http:// ones, so every HTTP tracker burned a UDP
// timeout and returned nothing.
func (tf *TorrentFile) AnnounceTo(trackerURL string, req AnnounceReq) (*AnnounceResp, error) {
	switch {
	case strings.HasPrefix(trackerURL, "udp://"):
		return tf.announceUDP(trackerURL, req)
	case strings.HasPrefix(trackerURL, "http://"), strings.HasPrefix(trackerURL, "https://"):
		return tf.announceHTTP(trackerURL, req)
	default:
		return nil, fmt.Errorf("unsupported tracker protocol: %s", trackerURL)
	}
}

func (tf *TorrentFile) announceHTTP(trackerURL string, r AnnounceReq) (*AnnounceResp, error) {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid tracker URL: %w", err)
	}

	params := url.Values{
		"info_hash":  []string{string(tf.InfoHash[:])},
		"peer_id":    []string{string(r.PeerID[:])},
		"port":       []string{strconv.Itoa(int(r.Port))},
		"uploaded":   []string{strconv.FormatInt(r.Uploaded, 10)},
		"downloaded": []string{strconv.FormatInt(r.Downloaded, 10)},
		"left":       []string{strconv.FormatInt(r.Left, 10)},
		"compact":    []string{"1"},
	}
	if ev := r.Event.String(); ev != "" {
		params.Set("event", ev)
	}
	u.RawQuery = params.Encode()

	timeout := 5 * time.Second
	if u.Scheme == "https" {
		timeout = 10 * time.Second // HTTPS needs more time for SSL handshake
	}
	client := &http.Client{Timeout: timeout}

	httpReq, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("User-Agent", "go-torrent/0.1")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tracker request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tracker returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTrackerResponse))
	if err != nil {
		return nil, fmt.Errorf("failed to read tracker response: %w", err)
	}

	// DecodeWithLength rather than Decode: some trackers append a trailing
	// newline, which a whole-input decode would reject.
	val, _, err := bencode.DecodeWithLength(body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tracker response: %w", err)
	}
	dict, ok := val.(map[string]bencode.Value)
	if !ok {
		return nil, fmt.Errorf("tracker response is not a dictionary")
	}

	// A refusal is a well-formed response, not a parse error. Reporting it as
	// one hid the actual reason (bad info hash, unregistered torrent, ...).
	if reason, err := bencode.GetString(dict, "failure reason"); err == nil {
		return nil, fmt.Errorf("tracker refused: %s", reason)
	}

	out := &AnnounceResp{}
	if n, err := bencode.GetInt(dict, "interval"); err == nil {
		out.Interval = time.Duration(n) * time.Second
	}
	if n, err := bencode.GetInt(dict, "complete"); err == nil {
		out.Seeders = int(n)
	}
	if n, err := bencode.GetInt(dict, "incomplete"); err == nil {
		out.Leechers = int(n)
	}

	peers, err := parsePeerField(dict["peers"])
	if err != nil {
		return nil, err
	}
	out.Peers = filterSelfPeer(peers, r.Port)

	return out, nil
}

// parsePeerField handles both the compact form (a byte string, 6 bytes per peer)
// and the original dictionary form some trackers still return.
func parsePeerField(val bencode.Value) ([]Peer, error) {
	switch v := val.(type) {
	case nil:
		return nil, nil

	case string:
		return parsePeers([]byte(v))

	case []bencode.Value:
		var peers []Peer
		for _, entry := range v {
			d, ok := entry.(map[string]bencode.Value)
			if !ok {
				continue
			}
			ipStr, err := bencode.GetString(d, "ip")
			if err != nil {
				continue
			}
			port, err := bencode.GetInt(d, "port")
			if err != nil || port <= 0 || port > 65535 {
				continue
			}
			ip := net.ParseIP(ipStr)
			if ip == nil {
				continue
			}
			peers = append(peers, Peer{IP: ip, Port: uint16(port)})
		}
		return peers, nil

	default:
		return nil, fmt.Errorf("unexpected peers field type %T", val)
	}
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
		// Copy rather than alias: peersBin may be a reused read buffer.
		ip := make(net.IP, 4)
		copy(ip, peersBin[offset:offset+4])
		peers[i].IP = ip
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

	ourIPs := getOurIPs()

	for _, peer := range peers {
		isSelf := false
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

	// Note: we can't easily get our public IP without an external service. The
	// tracker usually avoids sending us our own IP; filtering by port covers
	// the rest.
	if localIP := getOutboundIP(); localIP != nil {
		ips = append(ips, localIP)
	}

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
