package p2p

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/mse"
	"github.com/Vaivaswat2244/go-torrent/internal/peers"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

const (
	// keepAliveInterval is how often we send a keep-alive. Peers drop idle
	// connections, and a seeding session can be idle for a long time.
	keepAliveInterval = 90 * time.Second

	// idleTimeout is how long we wait for any message before assuming the peer
	// is gone. It must exceed a peer's own keep-alive interval.
	idleTimeout = 4 * time.Minute

	// writeTimeout bounds a single write, so a peer that stops reading cannot
	// wedge us indefinitely.
	writeTimeout = 30 * time.Second

	// pickInterval is how often a session re-checks for work and for a stalled
	// piece. Without it, a session that found the queue empty went idle for
	// good: a piece handed back later was never noticed, because nothing on
	// the connection prompted a second look.
	pickInterval = time.Second

	// outboundQueue is how many messages may be waiting to be written before
	// the session loop blocks. Blocking is intentional: it becomes TCP
	// backpressure rather than unbounded memory growth.
	outboundQueue = 64
)

// These are variables rather than constants only so tests can shorten them.
var (
	// snubTimeout is how long we wait for a block on outstanding requests
	// before deciding the peer has stopped sending ("snubbed" us) and handing
	// the piece to someone else. Without it, a peer that unchoked us and then
	// went quiet held its piece indefinitely.
	snubTimeout = 30 * time.Second

	// snubCooldown keeps a snubbing peer from immediately reclaiming the piece
	// it just failed to deliver. The connection stays up, so we can still
	// upload to it.
	snubCooldown = time.Minute
)

// BlockStore is the piece data a session serves from. *MultiFileWriter
// satisfies it.
type BlockStore interface {
	ReadBlock(pieceIndex, begin, length int) ([]byte, error)
}

// LogFunc receives human-readable session events.
type LogFunc func(format string, args ...interface{})

// SessionStats is a point-in-time view for the choker and the UI.
type SessionStats struct {
	Addr           string
	Uploaded       int64
	Downloaded     int64
	AmChoking      bool
	PeerInterested bool
}

// Session drives one peer connection in both directions.
//
// This replaces the old download-only Worker, which looped inside
// PieceWork.Download until a whole piece arrived and dropped every inbound
// Request, Interested and Cancel message on the floor — a design that
// structurally could not upload while downloading. Instead there is one read
// loop dispatching by message ID, exactly as libtorrent, Transmission and
// anacrolix/torrent all do, with download progress held as data rather than in
// a stack frame.
//
// Three goroutines: a reader feeding a channel, a writer draining one, and the
// run loop that owns all session state. Because only the run loop mutates
// state, the internals need no locking; the few fields the choker reads
// concurrently are atomics.
type Session struct {
	ctx    context.Context
	cancel context.CancelFunc

	client *peers.Client
	tf     *torrentfile.TorrentFile
	store  BlockStore
	logf   LogFunc

	numPieces   int
	ourBitfield *SafeBitfield
	peerHas     Bitfield

	workQueue chan *PieceWork
	results   chan *PieceResult

	// onInterest fires when the peer becomes interested, so the choker can
	// fill a free slot immediately instead of waiting out the round timer.
	onInterest func()

	incoming chan *peers.Message
	out      chan *peers.Message
	setChoke chan bool
	haveCh   chan int

	// Download progress for the piece currently in flight.
	current    *PieceWork
	buf        []byte
	downloaded int
	requested  int
	backlog    int

	// lastBlockAt is when the in-flight piece last made progress, and
	// noPickUntil holds off claiming work after this peer snubbed us.
	lastBlockAt time.Time
	noPickUntil time.Time

	// Read concurrently by the choker.
	uploaded       atomic.Int64
	downloadedTot  atomic.Int64
	amChoking      atomic.Bool
	peerInterested atomic.Bool
}

// NewSession wraps an already-handshaken connection.
func NewSession(
	ctx context.Context,
	client *peers.Client,
	tf *torrentfile.TorrentFile,
	ourBitfield *SafeBitfield,
	store BlockStore,
	workQueue chan *PieceWork,
	results chan *PieceResult,
	logf LogFunc,
) *Session {
	ctx, cancel := context.WithCancel(ctx)

	if logf == nil {
		logf = func(string, ...interface{}) {}
	}

	s := &Session{
		ctx:         ctx,
		cancel:      cancel,
		client:      client,
		tf:          tf,
		store:       store,
		logf:        logf,
		numPieces:   len(tf.PieceHashes),
		ourBitfield: ourBitfield,
		peerHas:     NewBitfield(len(tf.PieceHashes)),
		workQueue:   workQueue,
		results:     results,
		incoming:    make(chan *peers.Message, 16),
		out:         make(chan *peers.Message, outboundQueue),
		setChoke:    make(chan bool, 4),
		haveCh:      make(chan int, 64),
	}
	// BEP 3: we start out choking everyone.
	s.amChoking.Store(true)
	return s
}

// Addr is the peer's address, used as the swarm registry key.
func (s *Session) Addr() string {
	if a := s.client.RemoteAddr(); a != nil {
		return a.String()
	}
	return ""
}

// Stats returns a snapshot for the choker and UI.
func (s *Session) Stats() SessionStats {
	return SessionStats{
		Addr:           s.Addr(),
		Uploaded:       s.uploaded.Load(),
		Downloaded:     s.downloadedTot.Load(),
		AmChoking:      s.amChoking.Load(),
		PeerInterested: s.peerInterested.Load(),
	}
}

// SetChoking asks the session to choke or unchoke the peer. Called by the
// choker; never blocks.
func (s *Session) SetChoking(choking bool) {
	select {
	case s.setChoke <- choking:
	default:
	}
}

// NotifyHave tells the peer we acquired a piece. Never blocks.
func (s *Session) NotifyHave(index int) {
	select {
	case s.haveCh <- index:
	default:
	}
}

// SetOnInterest registers a callback fired when the peer becomes interested.
// It must not block.
func (s *Session) SetOnInterest(fn func()) { s.onInterest = fn }

// Close tears the session down.
func (s *Session) Close() { s.cancel() }

// Run drives the session until the peer goes away or the context is cancelled.
func (s *Session) Run() {
	defer s.cancel()
	defer s.client.Conn.Close()

	// A session that ends mid-piece must give the piece back, or nobody else
	// can ever fetch it and the download stalls just short of 100%.
	defer s.releasePiece()

	// Cancellation unblocks any in-flight read or write.
	stop := context.AfterFunc(s.ctx, func() { s.client.Conn.Close() })
	defer stop()

	go s.reader()
	go s.writer()

	// Tell the peer what we have. An all-zero bitfield is legal.
	s.enqueue(&peers.Message{ID: peers.MsgBitfield, Payload: s.ourBitfield.Snapshot()})

	// Only express interest if there is anything left for us to want.
	if s.ourBitfield.Count() < s.numPieces {
		s.client.AmInterested = true
		s.enqueue(peers.FormatInterested())
	}

	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()

	pick := time.NewTicker(pickInterval)
	defer pick.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return

		case msg, ok := <-s.incoming:
			if !ok {
				return
			}
			if err := s.handle(msg); err != nil {
				s.logf("peer %s: %v", s.Addr(), err)
				return
			}

		case choking := <-s.setChoke:
			s.applyChoke(choking)

		case index := <-s.haveCh:
			s.enqueue(peers.FormatHave(index))

		case <-keepAlive.C:
			// A nil message serializes to the 4-byte keep-alive.
			s.enqueue(nil)

		case now := <-pick.C:
			s.checkProgress(now)
		}
	}
}

// reader turns the blocking message stream into a channel the run loop can
// select over.
func (s *Session) reader() {
	defer close(s.incoming)

	for {
		s.client.Conn.SetReadDeadline(time.Now().Add(idleTimeout))

		msg, err := s.client.ReadMessage()
		if err != nil {
			return
		}
		if msg == nil {
			continue // keep-alive
		}

		select {
		case s.incoming <- msg:
		case <-s.ctx.Done():
			return
		}
	}
}

// writer serializes outbound messages.
func (s *Session) writer() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case msg := <-s.out:
			s.client.Conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := s.client.Conn.Write(msg.Serialize()); err != nil {
				s.cancel()
				return
			}
		}
	}
}

// enqueue queues a message for the writer. It blocks when the queue is full,
// which propagates backpressure to the peer rather than buffering without limit.
func (s *Session) enqueue(msg *peers.Message) {
	select {
	case s.out <- msg:
	case <-s.ctx.Done():
	}
}

func (s *Session) applyChoke(choking bool) {
	if choking == s.amChoking.Load() {
		return
	}
	s.amChoking.Store(choking)
	s.client.AmChoking = choking

	if choking {
		s.enqueue(peers.FormatChoke())
		return
	}
	s.enqueue(peers.FormatUnchoke())
}

// handle dispatches one inbound message.
func (s *Session) handle(msg *peers.Message) error {
	switch msg.ID {
	case peers.MsgChoke:
		s.client.PeerChoking = true
		// The peer discards our outstanding requests when it chokes us, and
		// may not unchoke us again for a long time. Holding on to the piece
		// would stop every other peer from fetching it, so give it back.
		// Blocks already received for it are discarded.
		s.releasePiece()

	case peers.MsgUnchoke:
		s.client.PeerChoking = false
		s.fillPipeline()

	case peers.MsgInterested:
		s.client.PeerInterested = true
		if !s.peerInterested.Swap(true) && s.onInterest != nil {
			s.onInterest()
		}

	case peers.MsgNotInterested:
		s.client.PeerInterested = false
		s.peerInterested.Store(false)

	case peers.MsgHave:
		index, err := peers.ParseHave(msg)
		if err != nil {
			return err
		}
		s.peerHas.SetPiece(index)
		// The peer may now hold something we were unable to ask anyone for.
		s.fillPipeline()

	case peers.MsgBitfield:
		// Keep our correctly sized slice and copy in what they sent.
		copy(s.peerHas, msg.Payload)
		s.fillPipeline()

	case peers.MsgRequest:
		return s.serveRequest(msg)

	case peers.MsgCancel:
		// We serve requests inline, so by the time a cancel arrives the block
		// has already been queued. Dropping it is harmless: the peer discards
		// duplicate blocks.

	case peers.MsgPiece:
		return s.receiveBlock(msg)
	}

	return nil
}

// pieceLength returns the length of a piece, accounting for the short final one.
func (s *Session) pieceLength(index int) int {
	if index == s.numPieces-1 {
		if remainder := s.tf.Length % s.tf.PieceLength; remainder != 0 {
			return remainder
		}
	}
	return s.tf.PieceLength
}

// serveRequest validates and answers a peer's block request.
//
// This is peer-controlled input that decides what we read off disk, so every
// field is checked before it reaches the store.
func (s *Session) serveRequest(msg *peers.Message) error {
	req, err := peers.ParseRequest(msg)
	if err != nil {
		return err
	}

	// Requests while choked are simply ignored; peers routinely send them just
	// after being choked, and it is not misbehaviour.
	if s.amChoking.Load() {
		return nil
	}

	if req.Index < 0 || req.Index >= s.numPieces {
		return fmt.Errorf("requested piece %d out of range", req.Index)
	}
	if req.Length <= 0 || req.Length > MaxBlockSize {
		return fmt.Errorf("requested block length %d out of range", req.Length)
	}
	if req.Begin < 0 || req.Begin+req.Length > s.pieceLength(req.Index) {
		return fmt.Errorf("requested block %d+%d exceeds piece %d", req.Begin, req.Length, req.Index)
	}
	if !s.ourBitfield.Has(req.Index) {
		return fmt.Errorf("requested piece %d which we do not have", req.Index)
	}

	data, err := s.store.ReadBlock(req.Index, req.Begin, req.Length)
	if err != nil {
		return fmt.Errorf("reading block for peer: %w", err)
	}

	s.enqueue(peers.FormatPiece(req.Index, req.Begin, data))
	s.uploaded.Add(int64(len(data)))
	return nil
}

// receiveBlock stores an inbound block and completes the piece when full.
func (s *Session) receiveBlock(msg *peers.Message) error {
	if s.current == nil {
		return nil // unsolicited or late block
	}

	// After a choke we give our piece back, but blocks the peer had already
	// sent can still arrive, possibly once we have moved on to another piece.
	// ParsePiece rejects a mismatched index as an error, which would drop a
	// healthy connection over stale data, so filter those out first.
	if len(msg.Payload) >= 4 && int(binary.BigEndian.Uint32(msg.Payload[0:4])) != s.current.Index {
		return nil
	}

	n, err := peers.ParsePiece(s.current.Index, s.buf, msg)
	if err != nil {
		return err
	}

	s.downloaded += n
	s.downloadedTot.Add(int64(n))
	s.lastBlockAt = time.Now()
	if s.backlog > 0 {
		s.backlog--
	}

	if s.downloaded < s.current.Length {
		s.fillPipeline()
		return nil
	}

	// Piece complete.
	work, buf := s.current, s.buf
	s.current, s.buf = nil, nil

	if err := work.CheckIntegrity(buf); err != nil {
		// Bad data: return the piece and drop the peer.
		s.requeue(work)
		return err
	}

	select {
	case s.results <- &PieceResult{Index: work.Index, Buf: buf}:
	case <-s.ctx.Done():
		return nil
	}

	s.fillPipeline()
	return nil
}

// fillPipeline keeps requests in flight, picking up a new piece when needed.
func (s *Session) fillPipeline() {
	if s.client.PeerChoking {
		return
	}

	if s.current == nil && !s.pickPiece() {
		return
	}

	for s.backlog < MaxBacklog && s.requested < s.current.Length {
		blockSize := MaxBlockSize
		if s.current.Length-s.requested < blockSize {
			blockSize = s.current.Length - s.requested
		}

		s.enqueue(peers.FormatRequest(s.current.Index, s.requested, blockSize))
		s.requested += blockSize
		s.backlog++
	}
}

// pickPiece claims a piece this peer actually has.
//
// Pieces the peer lacks go straight back on the queue. The scan is bounded by
// the queue length so a peer holding nothing we need falls through instead of
// spinning the queue, which the previous implementation did at roughly a
// thousand rotations a second.
func (s *Session) pickPiece() bool {
	if time.Now().Before(s.noPickUntil) {
		return false
	}

	for attempts := len(s.workQueue) + 1; attempts > 0; attempts-- {
		var work *PieceWork
		select {
		case w, ok := <-s.workQueue:
			if !ok {
				return false
			}
			work = w
		default:
			return false // queue empty: nothing left to download
		}

		if !s.peerHas.HasPiece(work.Index) {
			s.requeue(work)
			continue
		}

		s.current = work
		s.buf = make([]byte, work.Length)
		s.downloaded, s.requested, s.backlog = 0, 0, 0
		s.lastBlockAt = time.Now()
		return true
	}

	return false
}

// requeue returns a piece for another peer to take.
//
// The queue is sized to hold every piece, and a piece is only ever in one
// place, so while we hold one out there is always room and this cannot block.
// It used to select on the session context as well; when a session was ending
// both cases were ready, Go picked one at random, and the piece was lost about
// half the time.
func (s *Session) requeue(work *PieceWork) {
	select {
	case s.workQueue <- work:
	default:
		s.logf("peer %s: work queue full, piece %d dropped", s.Addr(), work.Index)
	}
}

// checkProgress runs on a timer. An idle session looks for work again, and a
// session whose peer has stopped delivering gives its piece away.
func (s *Session) checkProgress(now time.Time) {
	if s.current == nil {
		s.fillPipeline()
		return
	}

	if s.backlog > 0 && now.Sub(s.lastBlockAt) > snubTimeout {
		s.logf("peer %s: no data for %s, handing piece %d to another peer",
			s.Addr(), snubTimeout, s.current.Index)
		s.releasePiece()
		s.noPickUntil = now.Add(snubCooldown)
	}
}

// releasePiece hands the in-flight piece back and clears download progress.
func (s *Session) releasePiece() {
	if s.current != nil {
		s.requeue(s.current)
	}
	s.current, s.buf = nil, nil
	s.downloaded, s.requested, s.backlog = 0, 0, 0
}

// Dial opens an outgoing connection, handshakes, and returns a ready session.
func Dial(
	ctx context.Context,
	peer torrentfile.Peer,
	tf *torrentfile.TorrentFile,
	peerID [20]byte,
	ourBitfield *SafeBitfield,
	store BlockStore,
	workQueue chan *PieceWork,
	results chan *PieceResult,
	logf LogFunc,
	encryption mse.Policy,
) (*Session, error) {
	client, err := peers.Connect(ctx, peer.String(), tf.InfoHash, peerID, encryption)
	if err != nil {
		return nil, err
	}

	return NewSession(ctx, client, tf, ourBitfield, store, workQueue, results, logf), nil
}
