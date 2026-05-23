package dht

import (
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

func FindPeers(infoHash [20]byte, peerChan chan<- torrentfile.Peer) {
	nodeID := GenerateNodeID()

	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		fmt.Println("DHT: Failed to open UDP socket")
		return
	}
	defer conn.Close()

	nodeQueue := make(chan string, 5000)

	// seenNodes is now protected by a mutex since it's shared across goroutines
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

	fmt.Println("🌐 DHT Crawler starting...")

	// Reader goroutine
	go func() {
		buf := make([]byte, 2048)
		for {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
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
					if peerStr, ok := v.(string); ok {
						peers, _ := parseCompactPeers([]byte(peerStr))
						for _, p := range peers {
							select {
							case peerChan <- p:
							default:
							}
						}
					}
				}
			}

			// Got closer nodes — queue them up
			if nodesStr, err := bencode.GetString(rDict, "nodes"); err == nil {
				nodes, err := ParseCompactNodes(nodesStr)
				if err == nil {
					for _, n := range nodes {
						addNode(n.String())
					}
				}
			}
		}
	}()

	// Sender loop: re-encode with a fresh transaction ID per node
	// so DHT nodes don't discard duplicate t values
	for addr := range nodeQueue {
		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			continue
		}

		// Fresh query per node (new transaction ID each time)
		queryMap := FormatGetPeers(nodeID, infoHash)
		queryBytes, err := bencode.Encode(queryMap)
		if err != nil {
			continue
		}

		conn.WriteToUDP(queryBytes, udpAddr)

		// 5ms throttle — fast enough to saturate the queue, slow enough
		// that response goroutine can refill nodeQueue before we drain it
		time.Sleep(5 * time.Millisecond)
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
		peers[i].IP = net.IP(buf[offset : offset+4])
		peers[i].Port = uint16(buf[offset+4])<<8 | uint16(buf[offset+5])
	}

	return peers, nil
}
