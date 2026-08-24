package metadata

import (
	"context"
	"crypto/sha1"
	"fmt"
	"net"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/bencode"
	"github.com/Vaivaswat2244/go-torrent/internal/peers"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// maxMetadataSize caps the metadata_size a peer may advertise. numPieces is
// derived from it and drives an append loop, so an unbounded value lets a single
// peer drive us out of memory. Real info dictionaries are well under this.
const maxMetadataSize = 16 << 20 // 16 MiB

// Fetch coordinates the downloading of the .torrent metadata from peers
func Fetch(ctx context.Context, infoHash [20]byte, peerID [20]byte, peerChan <-chan torrentfile.Peer) ([]byte, error) {
	// Buffered so a verified result is never dropped. Previously this was
	// unbuffered with a "default:" send, so if the receiver was not parked at
	// that exact instant the metadata was thrown away and the worker exited.
	resultChan := make(chan []byte, 1)

	// Cancelled when Fetch returns, so the workers stop dialing peers instead
	// of leaking for the lifetime of the process.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := ctx.Done()

	// Launch 10 concurrent workers to try peers simultaneously
	for i := 0; i < 10; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				case peer, ok := <-peerChan:
					if !ok {
						return
					}

					infoBytes, err := tryFetchFromPeer(peer, infoHash, peerID)
					if err != nil {
						continue
					}

					// Verify the downloaded metadata matches our magnet link
					if sha1.Sum(infoBytes) != infoHash {
						continue
					}

					select {
					case resultChan <- infoBytes:
					default:
					}
					return
				}
			}
		}()
	}

	// Wait for the FIRST successful result, or timeout after 60 seconds
	select {
	case infoBytes := <-resultChan:
		return infoBytes, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("metadata fetch cancelled")
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

	// Previously "return nil, err" here, where err is nil on the normal break
	// path -- so a peer without ut_metadata produced (nil, nil), and the caller
	// treated that as success and SHA-1'd a nil slice.
	if theirMetadataID == 0 {
		return nil, fmt.Errorf("peer does not support ut_metadata")
	}
	if metadataSize <= 0 {
		return nil, fmt.Errorf("peer advertised metadata_size %d", metadataSize)
	}
	if metadataSize > maxMetadataSize {
		return nil, fmt.Errorf("peer advertised metadata_size %d, over the %d limit", metadataSize, maxMetadataSize)
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
