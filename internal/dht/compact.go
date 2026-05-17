package dht

import (
	"fmt"
	"net"

	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// ParseCompactNodes extracts DHT nodes from a 26-byte compact format
func ParseCompactNodes(compact string) ([]torrentfile.Peer, error) {
	buf := []byte(compact)
	const nodeSize = 26 // 20 (ID) + 4 (IP) + 2 (Port)

	if len(buf)%nodeSize != 0 {
		return nil, fmt.Errorf("invalid compact nodes length")
	}

	numNodes := len(buf) / nodeSize
	nodes := make([]torrentfile.Peer, numNodes)

	for i := 0; i < numNodes; i++ {
		offset := i * nodeSize

		// Bytes 0-19 are the Node ID (we ignore it for our simple crawler)
		// Bytes 20-23 are the IP
		nodes[i].IP = net.IP(buf[offset+20 : offset+24])
		// Bytes 24-25 are the Port
		nodes[i].Port = uint16(buf[offset+24])<<8 | uint16(buf[offset+25])
	}

	return nodes, nil
}
