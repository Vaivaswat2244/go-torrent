package dht

import (
	"crypto/rand"
)

// Msg represents a Bencoded DHT UDP message
type Msg struct {
	T string                 `bencode:"t"` // Transaction ID (2 random bytes)
	Y string                 `bencode:"y"` // Type: "q" (query), "r" (response), "e" (error)
	Q string                 `bencode:"q"` // Query type: "ping", "find_node", "get_peers", "announce_peer"
	A map[string]interface{} `bencode:"a"` // Query arguments
	R map[string]interface{} `bencode:"r"` // Response values
}

// GenerateNodeID creates a random 20-byte Kademlia Node ID
func GenerateNodeID() [20]byte {
	var id [20]byte
	rand.Read(id[:])
	return id
}

// FormatGetPeers creates a get_peers query
func FormatGetPeers(nodeID [20]byte, infoHash [20]byte) map[string]interface{} {
	// Generate a 2-byte random transaction ID
	tid := make([]byte, 2)
	rand.Read(tid)

	args := make(map[string]interface{})
	args["id"] = string(nodeID[:])
	args["info_hash"] = string(infoHash[:])

	// We construct this as a map that our bencode encoder can serialize
	msg := make(map[string]interface{})
	msg["t"] = string(tid)
	msg["y"] = "q"
	msg["q"] = "get_peers"
	msg["a"] = args

	return msg
}
