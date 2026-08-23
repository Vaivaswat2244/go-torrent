package dht

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/bencode"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

var BootstrapNodes = []string{
	"router.bittorrent.com:6881",
	"router.utorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"dht.aelitis.com:6881",
}

// LogFunc receives progress messages. The crawler used to fmt.Println straight
// to stdout, which corrupts the TUI's alt-screen.
type LogFunc func(format string, args ...interface{})

// FindPeers crawls the DHT for peers holding infoHash, delivering them to
// peerChan until ctx is cancelled.
//
// This is a flood crawler rather than a real Kademlia node: it queries every
// node it hears about instead of converging on the closest ones, and it neither
// announces itself nor answers incoming queries.
func FindPeers(ctx context.Context, infoHash [20]byte, peerChan chan<- torrentfile.Peer, logf LogFunc) {
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}

	nodeID := GenerateNodeID()

	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		logf("DHT: failed to open UDP socket: %v", err)
		return
	}
	defer conn.Close()

	// Previously the sender ranged over a channel nothing ever closed, so this
	// function never returned: the socket and both goroutines leaked, and the
	// crawl kept hammering the network for the life of the process.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	nodeQueue := make(chan string, 5000)

	var mu sync.Mutex
	seenNodes := make(map[string]bool)

	addNode := func(addr string) {
		mu.Lock()
		defer mu.Unlock()
		if !seenNodes[addr] {
			seenNodes[addr] = true
			select {
			case nodeQueue <- addr:
			default:
			}
		}
	}

	for _, addr := range BootstrapNodes {
		addNode(addr)
	}

	logf("DHT crawler starting")

	// Reader goroutine
	go func() {
		buf := make([]byte, 2048)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				// A closed socket means we are shutting down; anything else is
				// a timeout worth retrying.
				if ctx.Err() != nil {
					return
				}
				continue
			}

			val, err := bencode.Decode(buf[:n])
			if err != nil {
				continue
			}

			respMap, ok := val.(map[string]bencode.Value)
			if !ok {
				continue
			}

			y, _ := bencode.GetString(respMap, "y")
			if y != "r" {
				continue
			}

			rDict, err := bencode.GetDict(respMap, "r")
			if err != nil {
				continue
			}

			// Got peers directly
			if values, ok := rDict["values"].([]bencode.Value); ok {
				for _, v := range values {
					peerStr, ok := v.(string)
					if !ok {
						continue
					}
					peers, err := parseCompactPeers([]byte(peerStr))
					if err != nil {
						continue
					}
					for _, p := range peers {
						select {
						case peerChan <- p:
						case <-ctx.Done():
							return
						default:
						}
					}
				}
			}

			// Got closer nodes - queue them up
			if nodesStr, err := bencode.GetString(rDict, "nodes"); err == nil {
				if nodes, err := ParseCompactNodes(nodesStr); err == nil {
					for _, n := range nodes {
						addNode(n.String())
					}
				}
			}
		}
	}()

	// Sender loop: re-encode with a fresh transaction ID per node
	// so DHT nodes don't discard duplicate t values
	for {
		var addr string
		select {
		case <-ctx.Done():
			return
		case addr = <-nodeQueue:
		}

		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			continue
		}

		queryMap := FormatGetPeers(nodeID, infoHash)
		queryBytes, err := bencode.Encode(queryMap)
		if err != nil {
			continue
		}

		conn.WriteToUDP(queryBytes, udpAddr)

		// Throttle so we don't drain the queue faster than replies refill it.
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func parseCompactPeers(buf []byte) ([]torrentfile.Peer, error) {
	const peerSize = 6
	if len(buf)%peerSize != 0 {
		return nil, fmt.Errorf("invalid compact peer list length: %d", len(buf))
	}

	numPeers := len(buf) / peerSize
	peers := make([]torrentfile.Peer, numPeers)

	for i := 0; i < numPeers; i++ {
		offset := i * peerSize
		ip := make(net.IP, 4)
		copy(ip, buf[offset:offset+4])
		peers[i].IP = ip
		peers[i].Port = uint16(buf[offset+4])<<8 | uint16(buf[offset+5])
	}

	return peers, nil
}
