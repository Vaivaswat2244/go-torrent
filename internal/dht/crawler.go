package dht

import (
	"fmt"
	"net"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/bencode"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// BootstrapNodes are the global entry points into the DHT network
var BootstrapNodes = []string{
	"router.bittorrent.com:6881",
	"router.utorrent.com:6881",
	"dht.transmissionbt.com:6881",
}

// FindPeers crawls the DHT network looking for peers downloading the given InfoHash.
// It sends discovered peers to the peerChan.
func FindPeers(infoHash [20]byte, peerChan chan<- torrentfile.Peer) {
	nodeID := GenerateNodeID()

	// 1. Setup UDP Socket
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		fmt.Println("DHT: Failed to open UDP socket")
		return
	}
	defer conn.Close()

	// 2. Channels for our crawler loop
	nodeQueue := make(chan string, 1000)
	seenNodes := make(map[string]bool)

	// 3. Kickstart by pinging the bootstrap routers
	for _, addr := range BootstrapNodes {
		nodeQueue <- addr
		seenNodes[addr] = true
	}

	fmt.Println("🌐 DHT Crawler starting...")

	// 4. Start a goroutine to read UDP responses continuously
	go readResponses(conn, peerChan, nodeQueue, seenNodes)

	// 5. The Sending Loop: pull nodes from the queue and ask them for peers
	queryMap := FormatGetPeers(nodeID, infoHash)

	queryBytes, err := bencode.Encode(queryMap)

	if err != nil {
		fmt.Printf("DHT Error: Failed to encode query map: %v\n", err)
		return
	}

	for addr := range nodeQueue {
		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			continue
		}

		// Ask this node: "Do you know peers for my InfoHash?"
		conn.WriteToUDP(queryBytes, udpAddr)

		// Throttle slightly so we don't spam the network
		time.Sleep(10 * time.Millisecond)
	}
}

// readResponses listens for incoming DHT UDP packets
func readResponses(conn *net.UDPConn, peerChan chan<- torrentfile.Peer, nodeQueue chan<- string, seen map[string]bool) {
	buf := make([]byte, 2048)

	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		// Decode the bencoded response
		val, err := bencode.Decode(buf[:n])
		if err != nil {
			continue
		}

		respMap, ok := val.(map[string]bencode.Value)
		if !ok {
			continue
		}

		// Check if it's a response ("r")
		y, _ := bencode.GetString(respMap, "y")
		if y != "r" {
			continue
		}

		rDict, err := bencode.GetDict(respMap, "r")
		if err != nil {
			continue
		}

		// CASE 1: They gave us Peers! ("values")
		if values, ok := rDict["values"].([]bencode.Value); ok {
			for _, v := range values {
				if peerStr, ok := v.(string); ok {
					// "values" uses the standard 6-byte compact peer format
					// (You already wrote a parsePeers function for the tracker, we can reuse that logic!)
					// Let's assume you expose it or duplicate it here:
					peers, _ := parseCompactPeers([]byte(peerStr))
					for _, p := range peers {
						peerChan <- p
					}
				}
			}
		}

		// CASE 2: They gave us closer DHT Nodes! ("nodes")
		if nodesStr, err := bencode.GetString(rDict, "nodes"); err == nil {
			nodes, err := ParseCompactNodes(nodesStr)
			if err == nil {
				for _, n := range nodes {
					addr := n.String()
					if !seen[addr] {
						seen[addr] = true
						// Push the new node to our queue to interrogate it later!
						select {
						case nodeQueue <- addr:
						default: // Queue full, drop it
						}
					}
				}
			}
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

		// Extract 4 bytes for the IP address
		peers[i].IP = net.IP(buf[offset : offset+4])

		// Extract 2 bytes for the Port (Big-Endian conversion)
		peers[i].Port = uint16(buf[offset+4])<<8 | uint16(buf[offset+5])
	}

	return peers, nil
}
