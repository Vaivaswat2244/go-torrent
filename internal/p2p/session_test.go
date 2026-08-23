package p2p

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/peers"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// fakeStore serves deterministic bytes without touching a disk.
type fakeStore struct {
	data        []byte
	pieceLength int
	err         error
}

func (f *fakeStore) ReadBlock(pieceIndex, begin, length int) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	off := pieceIndex*f.pieceLength + begin
	if off+length > len(f.data) {
		return nil, net.ErrClosed
	}
	return f.data[off : off+length], nil
}

func sessionTorrent(pieceLength, numPieces int) *torrentfile.TorrentFile {
	hashes := make([][20]byte, numPieces)
	return &torrentfile.TorrentFile{
		Name:        "session",
		PieceLength: pieceLength,
		Length:      pieceLength * numPieces,
		PieceHashes: hashes,
	}
}

// harness wires a Session to an in-memory peer we can drive by hand.
type harness struct {
	session *Session
	peer    *peers.Client // the far side of the pipe
	cancel  context.CancelFunc
	done    chan struct{}
}

// newHarness builds a session that already holds every piece, i.e. a seeder.
func newHarness(t *testing.T, tf *torrentfile.TorrentFile, store BlockStore, complete bool) *harness {
	t.Helper()

	serverConn, clientConn := net.Pipe()

	var infoHash, serverID, clientID [20]byte
	copy(infoHash[:], "session-info-hash-20")
	copy(serverID[:], "-GT0001-serverserver")
	copy(clientID[:], "-GT0001-clientclient")

	// CompleteHandshake writes then reads; AcceptHandshake reads then writes.
	// net.Pipe is synchronous, so they have to run concurrently.
	type result struct {
		client *peers.Client
		err    error
	}
	peerCh := make(chan result, 1)
	go func() {
		c, err := peers.CompleteHandshake(clientConn, infoHash, clientID)
		peerCh <- result{c, err}
	}()

	server, err := peers.AcceptHandshake(serverConn, serverID, func(h [20]byte) bool {
		return h == infoHash
	})
	if err != nil {
		t.Fatalf("AcceptHandshake: %v", err)
	}

	got := <-peerCh
	if got.err != nil {
		t.Fatalf("CompleteHandshake: %v", got.err)
	}

	numPieces := len(tf.PieceHashes)
	bf := NewSafeBitfield(numPieces)
	if complete {
		for i := 0; i < numPieces; i++ {
			bf.Set(i)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := NewSession(ctx, server, tf, bf, store,
		make(chan *PieceWork, numPieces), make(chan *PieceResult, numPieces), nil)

	h := &harness{session: s, peer: got.client, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		s.Run()
	}()

	t.Cleanup(func() {
		cancel()
		clientConn.Close()
		<-h.done
	})

	return h
}

// expect reads until a message of the wanted ID arrives.
func (h *harness) expect(t *testing.T, id peers.MessageID, timeout time.Duration) *peers.Message {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h.peer.Conn.SetReadDeadline(deadline)
		msg, err := h.peer.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for message %d: %v", id, err)
		}
		if msg != nil && msg.ID == id {
			return msg
		}
	}

	t.Fatalf("timed out waiting for message ID %d", id)
	return nil
}

// A seeding session must answer a request from an unchoked peer.
func TestSessionServesRequest(t *testing.T) {
	const pieceLength = 4096
	tf := sessionTorrent(pieceLength, 4)

	data := make([]byte, tf.Length)
	for i := range data {
		data[i] = byte(i)
	}
	h := newHarness(t, tf, &fakeStore{data: data, pieceLength: pieceLength}, true)

	// The session announces what it has first.
	bf := h.expect(t, peers.MsgBitfield, 5*time.Second)
	if !Bitfield(bf.Payload).HasPiece(2) {
		t.Error("advertised bitfield should include piece 2")
	}

	// Become interested, then get unchoked.
	if err := h.peer.SendInterested(); err != nil {
		t.Fatal(err)
	}
	h.session.SetChoking(false)
	h.expect(t, peers.MsgUnchoke, 5*time.Second)

	// Ask for a block that straddles nothing special, at a non-zero offset.
	if err := h.peer.SendRequest(2, 1024, 512); err != nil {
		t.Fatal(err)
	}

	msg := h.expect(t, peers.MsgPiece, 5*time.Second)
	buf := make([]byte, pieceLength)
	n, err := peers.ParsePiece(2, buf, msg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 512 {
		t.Fatalf("got %d bytes, want 512", n)
	}

	want := data[2*pieceLength+1024 : 2*pieceLength+1024+512]
	if !bytes.Equal(buf[1024:1024+512], want) {
		t.Error("served block does not match the stored data")
	}

	if up := h.session.Stats().Uploaded; up != 512 {
		t.Errorf("Uploaded = %d, want 512", up)
	}
}

// While choking, requests must be ignored rather than served.
func TestSessionIgnoresRequestWhileChoking(t *testing.T) {
	const pieceLength = 4096
	tf := sessionTorrent(pieceLength, 2)

	h := newHarness(t, tf, &fakeStore{data: make([]byte, tf.Length), pieceLength: pieceLength}, true)
	h.expect(t, peers.MsgBitfield, 5*time.Second)

	// A session starts out choking, per BEP 3, so no unchoke is sent.
	if err := h.peer.SendRequest(0, 0, 512); err != nil {
		t.Fatal(err)
	}

	// Nothing should come back. A keep-alive or nothing at all is fine; a
	// Piece message is not.
	h.peer.Conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	for {
		msg, err := h.peer.ReadMessage()
		if err != nil {
			break // timed out, which is the expected outcome
		}
		if msg != nil && msg.ID == peers.MsgPiece {
			t.Fatal("session served a block while choking")
		}
	}

	if up := h.session.Stats().Uploaded; up != 0 {
		t.Errorf("Uploaded = %d while choking, want 0", up)
	}
}

// Requests are peer-controlled input that drive disk reads, so out-of-range
// values must drop the connection instead of reaching the store.
func TestSessionRejectsInvalidRequests(t *testing.T) {
	const pieceLength = 4096

	cases := map[string]struct{ index, begin, length int }{
		"piece index past the end": {99, 0, 512},
		"negative-looking index":   {-1, 0, 512},
		"block longer than max":    {0, 0, MaxBlockSize * 4},
		"block past piece end":     {0, pieceLength - 16, 512},
		"zero length":              {0, 0, 0},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tf := sessionTorrent(pieceLength, 2)
			store := &fakeStore{data: make([]byte, tf.Length), pieceLength: pieceLength}
			h := newHarness(t, tf, store, true)

			h.expect(t, peers.MsgBitfield, 5*time.Second)
			if err := h.peer.SendInterested(); err != nil {
				t.Fatal(err)
			}
			h.session.SetChoking(false)
			h.expect(t, peers.MsgUnchoke, 5*time.Second)

			if err := h.peer.SendRequest(tc.index, tc.begin, tc.length); err != nil {
				t.Fatal(err)
			}

			// The session should tear the connection down.
			select {
			case <-h.done:
			case <-time.After(5 * time.Second):
				t.Fatal("session accepted an invalid request instead of dropping the peer")
			}

			if up := h.session.Stats().Uploaded; up != 0 {
				t.Errorf("Uploaded = %d, want 0 for a rejected request", up)
			}
		})
	}
}

// A session must not serve a piece it does not hold.
func TestSessionRefusesPieceItLacks(t *testing.T) {
	const pieceLength = 4096
	tf := sessionTorrent(pieceLength, 2)

	// complete=false, so the bitfield is empty.
	h := newHarness(t, tf, &fakeStore{data: make([]byte, tf.Length), pieceLength: pieceLength}, false)
	h.expect(t, peers.MsgBitfield, 5*time.Second)

	if err := h.peer.SendInterested(); err != nil {
		t.Fatal(err)
	}
	h.session.SetChoking(false)
	h.expect(t, peers.MsgUnchoke, 5*time.Second)

	if err := h.peer.SendRequest(0, 0, 512); err != nil {
		t.Fatal(err)
	}

	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not drop a peer requesting a piece we lack")
	}
}

// Interest must be reported to the choker.
func TestSessionReportsInterest(t *testing.T) {
	tf := sessionTorrent(4096, 2)
	h := newHarness(t, tf, &fakeStore{data: make([]byte, tf.Length), pieceLength: 4096}, true)
	h.expect(t, peers.MsgBitfield, 5*time.Second)

	fired := make(chan struct{}, 1)
	h.session.SetOnInterest(func() { fired <- struct{}{} })

	if err := h.peer.SendInterested(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("onInterest never fired")
	}

	if !h.session.Stats().PeerInterested {
		t.Error("PeerInterested not recorded")
	}
}
