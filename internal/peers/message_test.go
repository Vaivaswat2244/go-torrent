package peers

import (
	"bytes"
	"encoding/binary"
	"testing"
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
