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
func Fetch(infoHash [20]byte, peerID [20]byte, peerChan <-chan torrentfile.Peer) ([]byte, error) {

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
						select {
						case resultChan <- infoBytes:
						default:
						}
						return
					} else {
						fmt.Printf("  [%s] hash mismatch — got %x\n", peer, hash)
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
	extHandshake := map[string]interface{}{
		"m": map[string]interface{}{
			"ut_metadata": 1,
		},
	}
	encodedHandshake, _ := bencode.Encode(extHandshake)
	payload := append([]byte{0}, encodedHandshake...)
	client.SendExtendedMessage(payload)

	// 3. Wait for their Extended Handshake
	var theirMetadataID int64
	var metadataSize int64

	client.Conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	for i := 0; i < 150; i++ {
		msg, err := client.ReadMessage()
		if err != nil {
			break
		}
		if msg == nil {
			continue
		}

		if msg.ID == peers.MsgExtended && len(msg.Payload) > 1 {
			extID := msg.Payload[0]
			if extID == 0 {
				dict, err := bencode.Decode(msg.Payload[1:])
				if err != nil {
					continue
				}

				respMap, ok := dict.(map[string]bencode.Value)
				if !ok {
					continue
				}

				if size, err := bencode.GetInt(respMap, "metadata_size"); err == nil {
					metadataSize = size
				}

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
		return nil, err
	}

	// 4. Request Metadata Pieces (BEP 9)
	client.Conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	numPieces := (metadataSize + 16383) / 16384
	var rawInfo []byte

	for piece := int64(0); piece < numPieces; piece++ {
		reqDict := map[string]interface{}{
			"msg_type": 0,
			"piece":    piece,
		}
		encodedReq, _ := bencode.Encode(reqDict)
		reqPayload := append([]byte{byte(theirMetadataID)}, encodedReq...)
		client.SendExtendedMessage(reqPayload)

		var msg *peers.Message
		for {
			m, err := client.ReadMessage()
			if err != nil {
				return nil, err
			}
			if m == nil {
				continue // keep-alive
			}
			if m.ID == peers.MsgExtended {
				msg = m
				break
			}
			// Skip Bitfield, Have, Unchoke, etc.
		}

		// The response is: [Extended ID byte] + [Bencoded dict] + [Raw piece data]
		// Use proper bencode parsing to find exactly where the dict ends
		// instead of searching for "ee" which can appear in binary data
		payload := msg.Payload[1:] // strip the extended msg ID byte
		_, consumed, err := bencode.DecodeWithLength(payload)
		if err != nil {
			return nil, err
		}

		pieceData := payload[consumed:]
		rawInfo = append(rawInfo, pieceData...)
	}

	return rawInfo, nil
}
