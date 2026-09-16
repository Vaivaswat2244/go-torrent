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

// leechHarness is a session that still needs every piece, talking to a peer we
// drive by hand. It exposes the work queue so tests can see where pieces go.
type leechHarness struct {
	session   *Session
	peer      *peers.Client
	workQueue chan *PieceWork
	done      chan struct{}
	numPieces int
}

func newLeechHarness(t *testing.T, numPieces int) *leechHarness {
	t.Helper()

	const pieceLength = 4096
	tf := sessionTorrent(pieceLength, numPieces)

	serverConn, clientConn := net.Pipe()

	var infoHash, ours, theirs [20]byte
	copy(infoHash[:], "leech-info-hash-2020")
	copy(ours[:], "-GT0001-leecherleech")
	copy(theirs[:], "-GT0001-seederseeder")

	peerCh := make(chan *peers.Client, 1)
	go func() {
		c, err := peers.AcceptHandshake(clientConn, theirs, func(h [20]byte) bool { return h == infoHash })
		if err != nil {
			peerCh <- nil
			return
		}
		peerCh <- c
	}()

	client, err := peers.CompleteHandshake(serverConn, infoHash, ours)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	peer := <-peerCh
	if peer == nil {
		t.Fatal("peer handshake failed")
	}

	workQueue := make(chan *PieceWork, numPieces)
	for i := 0; i < numPieces; i++ {
		workQueue <- &PieceWork{Index: i, Length: pieceLength}
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := NewSession(ctx, client, tf, NewSafeBitfield(numPieces),
		&fakeStore{data: make([]byte, tf.Length), pieceLength: pieceLength},
		workQueue, make(chan *PieceResult, numPieces), nil)

	h := &leechHarness{session: s, peer: peer, workQueue: workQueue, done: make(chan struct{}), numPieces: numPieces}
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

// startDownloading makes the peer advertise every piece and unchoke us, then
// returns the first block request the session sends.
func (h *leechHarness) startDownloading(t *testing.T) peers.BlockRequest {
	t.Helper()

	all := NewBitfield(h.numPieces)
	for i := 0; i < h.numPieces; i++ {
		all.SetPiece(i)
	}
	if err := h.peer.SendBitfield(all); err != nil {
		t.Fatal(err)
	}
	if err := h.peer.SendUnchoke(); err != nil {
		t.Fatal(err)
	}

	req, err := peers.ParseRequest(h.expectMsg(t, peers.MsgRequest))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func (h *leechHarness) expectMsg(t *testing.T, id peers.MessageID) *peers.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
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
	t.Fatalf("timed out waiting for message %d", id)
	return nil
}

// waitForQueue polls until the work queue holds want pieces.
func (h *leechHarness) waitForQueue(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.workQueue) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("work queue holds %d pieces, want %d", len(h.workQueue), want)
}

// A peer that disconnects mid-piece must not take the piece with it. This used
// to strand the piece for good, and the download would sit just short of 100%.
func TestSessionReturnsPieceWhenPeerDisconnects(t *testing.T) {
	h := newLeechHarness(t, 4)
	h.expectMsg(t, peers.MsgBitfield)

	h.startDownloading(t)
	// One piece is now claimed by the session.
	h.waitForQueue(t, h.numPieces-1)

	h.peer.Conn.Close()

	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end after the peer disconnected")
	}

	if got := len(h.workQueue); got != h.numPieces {
		t.Fatalf("after disconnect the queue holds %d pieces, want all %d", got, h.numPieces)
	}
}

// A choked session must give its piece back so another peer can fetch it.
func TestSessionReleasesPieceWhenChoked(t *testing.T) {
	h := newLeechHarness(t, 4)
	h.expectMsg(t, peers.MsgBitfield)

	h.startDownloading(t)
	h.waitForQueue(t, h.numPieces-1)

	if err := h.peer.SendChoke(); err != nil {
		t.Fatal(err)
	}
	h.waitForQueue(t, h.numPieces)

	select {
	case <-h.done:
		t.Fatal("being choked should not end the session")
	default:
	}
}

// Blocks for a piece we have already given up are stale, not a protocol
// violation, and must not cost us the connection.
func TestSessionIgnoresStaleBlocks(t *testing.T) {
	h := newLeechHarness(t, 4)
	h.expectMsg(t, peers.MsgBitfield)

	first := h.startDownloading(t)

	// Choke, then unchoke: the session drops its piece and claims one again.
	if err := h.peer.SendChoke(); err != nil {
		t.Fatal(err)
	}
	h.waitForQueue(t, h.numPieces)
	if err := h.peer.SendUnchoke(); err != nil {
		t.Fatal(err)
	}
	next, err := peers.ParseRequest(h.expectMsg(t, peers.MsgRequest))
	if err != nil {
		t.Fatal(err)
	}

	// A block for a piece other than the one now in flight.
	stale := (next.Index + 1) % h.numPieces
	if err := h.peer.SendPiece(stale, first.Begin, make([]byte, 16)); err != nil {
		t.Fatal(err)
	}

	// The session must still be alive and responsive.
	time.Sleep(300 * time.Millisecond)
	select {
	case <-h.done:
		t.Fatal("a stale block ended the session")
	default:
	}
	if err := h.peer.SendMessage(peers.FormatHave(0)); err != nil {
		t.Fatalf("session stopped reading after a stale block: %v", err)
	}
}

// An idle session must notice a piece that is handed back to the queue later,
// even though nothing new arrives on its connection. It used to only look for
// work when a message came in, so a returned piece could sit unclaimed while
// every session waited — the download stalled one piece short.
func TestIdleSessionPicksUpReturnedPiece(t *testing.T) {
	h := newLeechHarness(t, 4)
	h.expectMsg(t, peers.MsgBitfield)

	// Another session is holding every piece.
	var held []*PieceWork
	for len(h.workQueue) > 0 {
		held = append(held, <-h.workQueue)
	}

	all := NewBitfield(h.numPieces)
	for i := 0; i < h.numPieces; i++ {
		all.SetPiece(i)
	}
	if err := h.peer.SendBitfield(all); err != nil {
		t.Fatal(err)
	}
	if err := h.peer.SendUnchoke(); err != nil {
		t.Fatal(err)
	}

	// Nothing to fetch yet, so no request should go out.
	h.peer.Conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	for {
		msg, err := h.peer.ReadMessage()
		if err != nil {
			break
		}
		if msg != nil && msg.ID == peers.MsgRequest {
			t.Fatal("session requested a block with an empty work queue")
		}
	}

	// The other session gives one piece back. The peer sends nothing new.
	returned := held[2]
	h.workQueue <- returned

	req, err := peers.ParseRequest(h.expectMsg(t, peers.MsgRequest))
	if err != nil {
		t.Fatal(err)
	}
	if req.Index != returned.Index {
		t.Fatalf("requested piece %d, want the returned piece %d", req.Index, returned.Index)
	}
}

// A peer that unchokes us and then stops sending must not hold its piece
// forever.
func TestSnubbedSessionGivesUpPiece(t *testing.T) {
	// Registered before the harness, so it runs after the session has stopped.
	oldTimeout, oldCooldown := snubTimeout, snubCooldown
	snubTimeout, snubCooldown = 300*time.Millisecond, time.Minute
	t.Cleanup(func() { snubTimeout, snubCooldown = oldTimeout, oldCooldown })

	h := newLeechHarness(t, 4)
	h.expectMsg(t, peers.MsgBitfield)

	h.startDownloading(t)
	h.waitForQueue(t, h.numPieces-1)

	// The peer never answers. After the timeout the piece must come back, and
	// the cooldown stops the session from immediately taking it again.
	h.waitForQueue(t, h.numPieces)
	time.Sleep(1500 * time.Millisecond)
	if got := len(h.workQueue); got != h.numPieces {
		t.Fatalf("snubbed session reclaimed a piece during its cooldown (queue=%d)", got)
	}

	select {
	case <-h.done:
		t.Fatal("a snubbing peer should stay connected, so we can still upload to it")
	default:
	}
}
