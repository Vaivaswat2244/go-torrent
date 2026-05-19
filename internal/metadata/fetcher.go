package metadata

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"net"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/bencode"
	"github.com/Vaivaswat2244/go-torrent/internal/peers"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// Fetch coordinates the downloading of the .torrent metadata from peers
func Fetch(infoHash [20]byte, peerID [20]byte, peerChan chan torrentfile.Peer) ([]byte, error) {
	fmt.Println("🔍 Searching the DHT for metadata... (racing peers)")

	resultChan := make(chan []byte)

	// Launch 10 concurrent workers to try peers simultaneously
	for i := 0; i < 10; i++ {
		go func() {
			for peer := range peerChan {
				infoBytes, err := tryFetchFromPeer(peer, infoHash, peerID)
				if err == nil {
					// Verify the downloaded metadata matches our magnet link
					hash := sha1.Sum(infoBytes)
					if bytes.Equal(hash[:], infoHash[:]) {
						// Non-blocking send in case multiple workers succeed at the exact same time
						select {
						case resultChan <- infoBytes:
						default:
						}
						return
					}
				}
			}
		}()
	}

	// Wait for the FIRST successful result, or timeout after 60 seconds
	select {
	case infoBytes := <-resultChan:
		fmt.Println("\n✅ Metadata downloaded and verified successfully!")
		return infoBytes, nil
	case <-time.After(60 * time.Second):
		return nil, fmt.Errorf("timed out: could not find any active peers with this metadata")
	}
}

func tryFetchFromPeer(peer torrentfile.Peer, infoHash, peerID [20]byte) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", peer.String(), 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// 1. Standard Handshake
	client, err := peers.CompleteHandshake(conn, infoHash, peerID)
	if err != nil {
		return nil, err
	}

	// 2. Send Extended Handshake (BEP 10)
	// We tell them our ut_metadata ID is 1
	extHandshake := map[string]interface{}{
		"m": map[string]interface{}{
			"ut_metadata": 1,
		},
	}
	encodedHandshake, _ := bencode.Encode(extHandshake)

	// Payload: [ExtendedMsgID (1 byte)] + [Bencoded Dict]
	// ID 0 represents the Initial Extended Handshake
	payload := append([]byte{0}, encodedHandshake...)
	client.SendExtendedMessage(payload)

	// 3. Wait for their Extended Handshake
	var theirMetadataID int64
	var metadataSize int64

	client.Conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	for i := 0; i < 150; i++ {
		msg, err := client.ReadMessage()
		if err != nil {
			return nil, err
		}
		if msg == nil {
			continue
		}

		if msg.ID == peers.MsgExtended && len(msg.Payload) > 1 {
			extID := msg.Payload[0]
			if extID == 0 { // They sent us an Extended Handshake!
				dict, err := bencode.Decode(msg.Payload[1:])
				if err != nil {
					continue
				}

				respMap, ok := dict.(map[string]bencode.Value)
				if !ok {
					continue
				}

				// Extract metadata size
				if size, err := bencode.GetInt(respMap, "metadata_size"); err == nil {
					metadataSize = size
				}

				// Extract their ut_metadata ID
				if mDict, err := bencode.GetDict(respMap, "m"); err == nil {
					if id, err := bencode.GetInt(mDict, "ut_metadata"); err == nil {
						theirMetadataID = id
						break
					}
				}
			}
		}
	}

	if metadataSize == 0 || theirMetadataID == 0 {
		return nil, fmt.Errorf("peer does not support ut_metadata")
	}

	// 4. Request the Metadata Pieces (BEP 9)
	// Standard metadata piece size is 16KB (16384 bytes)
	client.Conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	numPieces := (metadataSize + 16383) / 16384
	var rawInfo []byte

	for piece := int64(0); piece < numPieces; piece++ {
		reqDict := map[string]interface{}{
			"msg_type": 0, // 0 = request
			"piece":    piece,
		}
		encodedReq, _ := bencode.Encode(reqDict)

		// Payload: [Their Metadata ID] + [Bencoded Request Dict]
		reqPayload := append([]byte{byte(theirMetadataID)}, encodedReq...)
		client.SendExtendedMessage(reqPayload)

		// Read the piece response
		msg, err := client.ReadMessage()
		if err != nil || msg == nil || msg.ID != peers.MsgExtended {
			return nil, fmt.Errorf("failed to get metadata piece")
		}

		// The response payload is: [Extended ID] + [Bencoded Dict] + [Raw Binary Data]
		// Bencode natively stops parsing exactly where the dict ends,
		// leaving the raw binary data right after it.
		// For our simple crawler, we will do a fast byte-search for the end of the dict ("ee").
		idx := bytes.Index(msg.Payload[1:], []byte("ee"))
		if idx == -1 {
			return nil, fmt.Errorf("invalid metadata piece response")
		}

		// The binary data starts right after the "ee"
		pieceData := msg.Payload[1+idx+2:]
		rawInfo = append(rawInfo, pieceData...)
	}

	return rawInfo, nil
}
