package peers

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// A peer supplies the 4-byte length prefix, so an absurd value must be refused
// rather than turned into a multi-gigabyte allocation.
func TestReadMessageRejectsOversizedLength(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 0xFFFFFFFF)

	if _, err := ReadMessage(bytes.NewReader(header[:])); err == nil {
		t.Fatal("ReadMessage accepted a 4 GiB length prefix")
	}

	binary.BigEndian.PutUint32(header[:], MaxMessageSize+1)
	if _, err := ReadMessage(bytes.NewReader(header[:])); err == nil {
		t.Fatal("ReadMessage accepted a length above MaxMessageSize")
	}
}

func TestReadMessageKeepAlive(t *testing.T) {
	msg, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0, 0}))
	if err != nil {
		t.Fatal(err)
	}
	if msg != nil {
		t.Fatalf("keep-alive should decode to nil, got %v", msg)
	}
}

func TestSerializeRoundTrip(t *testing.T) {
	want := &Message{ID: MsgRequest, Payload: []byte{1, 2, 3, 4}}

	got, err := ReadMessage(bytes.NewReader(want.Serialize()))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("got %v/%v, want %v/%v", got.ID, got.Payload, want.ID, want.Payload)
	}
}

func TestParsePieceBounds(t *testing.T) {
	buf := make([]byte, 32)

	mk := func(index, begin int, data []byte) *Message {
		p := make([]byte, 8+len(data))
		binary.BigEndian.PutUint32(p[0:4], uint32(index))
		binary.BigEndian.PutUint32(p[4:8], uint32(begin))
		copy(p[8:], data)
		return &Message{ID: MsgPiece, Payload: p}
	}

	t.Run("valid", func(t *testing.T) {
		n, err := ParsePiece(1, buf, mk(1, 0, []byte{9, 9, 9}))
		if err != nil || n != 3 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if !bytes.Equal(buf[:3], []byte{9, 9, 9}) {
			t.Fatalf("buf = %v", buf[:3])
		}
	})

	bad := map[string]*Message{
		"wrong index":      mk(2, 0, []byte{1}),
		"begin past end":   mk(1, 64, []byte{1}),
		"data overruns":    mk(1, 30, make([]byte, 16)),
		"short payload":    {ID: MsgPiece, Payload: []byte{0, 0}},
		"wrong message id": {ID: MsgHave, Payload: make([]byte, 12)},
	}
	for name, msg := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePiece(1, buf, msg); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseHave(t *testing.T) {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, 1234)

	index, err := ParseHave(&Message{ID: MsgHave, Payload: payload})
	if err != nil || index != 1234 {
		t.Fatalf("index=%d err=%v", index, err)
	}

	if _, err := ParseHave(&Message{ID: MsgHave, Payload: []byte{1, 2}}); err == nil {
		t.Fatal("expected an error for a short Have payload")
	}
	if _, err := ParseHave(&Message{ID: MsgPiece, Payload: payload}); err == nil {
		t.Fatal("expected an error for the wrong message ID")
	}
}

// The extension bit must survive serialization or peers never offer ut_metadata
// and magnet links cannot resolve.
func TestHandshakeSetsExtensionBit(t *testing.T) {
	var infoHash, peerID [20]byte
	copy(infoHash[:], "aaaaaaaaaaaaaaaaaaaa")
	copy(peerID[:], "bbbbbbbbbbbbbbbbbbbb")

	buf := New(infoHash, peerID).Serialize()
	if len(buf) != 68 {
		t.Fatalf("handshake is %d bytes, want 68", len(buf))
	}
	if buf[25]&0x10 == 0 {
		t.Error("BEP 10 extension bit is not set")
	}

	got, err := Read(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	if got.InfoHash != infoHash || got.PeerID != peerID {
		t.Error("handshake did not round-trip")
	}
}

func TestReadHandshakeRejectsBadProtocol(t *testing.T) {
	buf := make([]byte, 68)
	buf[0] = 19
	copy(buf[1:20], "NotBitTorrent proto")

	if _, err := Read(bytes.NewReader(buf)); err == nil {
		t.Fatal("Read accepted a bad protocol string")
	}
}

func TestParseRequest(t *testing.T) {
	mk := func(id MessageID, index, begin, length uint32) *Message {
		p := make([]byte, 12)
		binary.BigEndian.PutUint32(p[0:4], index)
		binary.BigEndian.PutUint32(p[4:8], begin)
		binary.BigEndian.PutUint32(p[8:12], length)
		return &Message{ID: id, Payload: p}
	}

	for _, id := range []MessageID{MsgRequest, MsgCancel} {
		req, err := ParseRequest(mk(id, 7, 16384, 16384))
		if err != nil {
			t.Fatalf("id %d: %v", id, err)
		}
		if req.Index != 7 || req.Begin != 16384 || req.Length != 16384 {
			t.Fatalf("got %+v", req)
		}
	}

	// Huge values must be refused before anyone does arithmetic on them.
	if _, err := ParseRequest(mk(MsgRequest, 0xFFFFFFFF, 0, 16384)); err == nil {
		t.Error("accepted an out-of-range index")
	}
	if _, err := ParseRequest(mk(MsgRequest, 0, 0xFFFFFFFF, 16384)); err == nil {
		t.Error("accepted an out-of-range begin")
	}
	if _, err := ParseRequest(&Message{ID: MsgRequest, Payload: []byte{1, 2, 3}}); err == nil {
		t.Error("accepted a short payload")
	}
	if _, err := ParseRequest(&Message{ID: MsgPiece, Payload: make([]byte, 12)}); err == nil {
		t.Error("accepted the wrong message ID")
	}
}

func TestFormatPieceRoundTrip(t *testing.T) {
	data := []byte("block contents")

	msg := FormatPiece(3, 64, data)
	if msg.ID != MsgPiece {
		t.Fatalf("ID = %d", msg.ID)
	}

	// Round-trip through the wire encoding, as a peer would see it.
	decoded, err := ReadMessage(bytes.NewReader(msg.Serialize()))
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 128)
	n, err := ParsePiece(3, buf, decoded)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(data) || !bytes.Equal(buf[64:64+len(data)], data) {
		t.Fatalf("round trip lost data: n=%d buf=%q", n, buf[64:64+len(data)])
	}
}

func TestFormatChokeUnchokeCancel(t *testing.T) {
	if m := FormatChoke(); m.ID != MsgChoke || len(m.Payload) != 0 {
		t.Errorf("choke = %+v", m)
	}
	if m := FormatUnchoke(); m.ID != MsgUnchoke || len(m.Payload) != 0 {
		t.Errorf("unchoke = %+v", m)
	}

	req, err := ParseRequest(FormatCancel(1, 2, 3))
	if err != nil {
		t.Fatal(err)
	}
	if req.Index != 1 || req.Begin != 2 || req.Length != 3 {
		t.Errorf("cancel round trip = %+v", req)
	}
}

// The reserved bytes carry extension support and used to be discarded on read.
func TestHandshakeReservedBytes(t *testing.T) {
	var infoHash, peerID [20]byte
	buf := New(infoHash, peerID).Serialize()

	got, err := Read(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	if !got.SupportsExtensions() {
		t.Error("our own handshake should advertise extension support")
	}

	// A peer that sets no reserved bits.
	plain := make([]byte, 68)
	plain[0] = 19
	copy(plain[1:20], "BitTorrent protocol")

	got, err = Read(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	if got.SupportsExtensions() {
		t.Error("reported extension support for a peer that set no bits")
	}
}

// AcceptHandshake is the inbound mirror: read first to learn the torrent, then
// answer only if we serve it.
func TestAcceptHandshake(t *testing.T) {
	var wanted, other, ourID, theirID [20]byte
	copy(wanted[:], "wanted-info-hash-abc")
	copy(other[:], "some-other-info-hash")
	copy(ourID[:], "-GT0001-ourpeeridxx")
	copy(theirID[:], "-GT0001-theirpeerid")

	t.Run("accepts a torrent we serve", func(t *testing.T) {
		server, client := net.Pipe()
		defer client.Close()

		type res struct {
			c   *Client
			err error
		}
		ch := make(chan res, 1)
		go func() {
			c, err := AcceptHandshake(server, ourID, func(h [20]byte) bool { return h == wanted })
			ch <- res{c, err}
		}()

		peer, err := CompleteHandshake(client, wanted, theirID)
		if err != nil {
			t.Fatal(err)
		}
		got := <-ch
		if got.err != nil {
			t.Fatal(got.err)
		}

		if !got.c.Incoming {
			t.Error("accepted session should be marked Incoming")
		}
		if got.c.PeerID != theirID {
			t.Error("peer ID not recorded from the handshake")
		}
		if !got.c.AmChoking || !got.c.PeerChoking {
			t.Error("both sides should start choked per BEP 3")
		}
		if got.c.AmInterested || got.c.PeerInterested {
			t.Error("both sides should start uninterested")
		}
		if peer.PeerID != ourID {
			t.Error("dialer did not receive our peer ID")
		}
	})

	t.Run("refuses a torrent we do not serve", func(t *testing.T) {
		server, client := net.Pipe()
		defer client.Close()

		ch := make(chan error, 1)
		go func() {
			_, err := AcceptHandshake(server, ourID, func(h [20]byte) bool { return h == wanted })
			ch <- err
		}()

		// Send a handshake for a different torrent.
		hs := make([]byte, 68)
		hs[0] = 19
		copy(hs[1:20], "BitTorrent protocol")
		copy(hs[28:48], other[:])
		copy(hs[48:68], theirID[:])
		client.SetWriteDeadline(time.Now().Add(2 * time.Second))
		client.Write(hs)

		if err := <-ch; err == nil {
			t.Fatal("AcceptHandshake accepted an info hash we do not serve")
		}
	})
}
