package engine

import (
	"context"
	"crypto/sha1"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/dht"
	"github.com/Vaivaswat2244/go-torrent/internal/mse"
	"github.com/Vaivaswat2244/go-torrent/internal/p2p"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

type Status string

const (
	StatusStarting    Status = "Starting"
	StatusVerifying   Status = "Verifying"
	StatusDownloading Status = "Downloading"
	StatusStalled     Status = "Stalled"
	StatusSeeding     Status = "Seeding"
	StatusError       Status = "Error"
	StatusStopped     Status = "Stopped"
)

const (
	// stallAfter is how long without a completed piece before we report a stall
	// and re-announce to look for fresh peers.
	stallAfter = 2 * time.Minute

	// minAnnounceInterval floors what a tracker can ask us to do, so a tracker
	// advertising a tiny interval can't turn us into a hammer.
	minAnnounceInterval = 5 * time.Minute

	// rateWindow is the period transfer rates are averaged over.
	rateWindow = 15 * time.Second

	// DefaultMaxPeers caps concurrent connections per torrent. Real clients sit
	// in this range; connecting to every peer a tracker returns wastes sockets
	// and goroutines for no throughput gain.
	DefaultMaxPeers = 50
)

// Options configures a torrent. The zero value is usable: default peer cap,
// no seeding limits, and encryption preferred.
type Options struct {
	MaxPeers int

	// Seeding stops once either limit is reached. Zero means unlimited.
	SeedRatio float64
	SeedTime  time.Duration

	// Encryption controls MSE. PolicyPrefer, the zero value, tries an
	// encrypted connection first and falls back to plain BitTorrent.
	Encryption mse.Policy
}

type TorrentStats struct {
	Name          string
	Progress      float64
	SpeedBps      float64
	UploadBps     float64
	Status        Status
	PeersActive   int
	PeersUnchoked int
	Downloaded    int64
	Uploaded      int64
	Ratio         float64
	Total         int64
	ETA           time.Duration
}

type sample struct {
	at   time.Time
	down int64
	up   int64
}

type Torrent struct {
	TF     *torrentfile.TorrentFile
	Writer *p2p.MultiFileWriter

	bitfield *p2p.SafeBitfield
	swarm    *swarm
	maxPeers int
	opts     Options

	workQueue chan *p2p.PieceWork
	results   chan *p2p.PieceResult
	peerChan  chan torrentfile.Peer

	mu          sync.RWMutex
	status      Status
	piecesDone  int
	totalPieces int
	downloaded  int64
	verified    int
	lastPieceAt time.Time
	samples     []sample
	errText     string
	listener    net.Listener

	// sessions tracks live peer sessions so shutdown can wait for them before
	// closing the files they read from.
	sessions sync.WaitGroup

	events chan string

	// completed marks the download finished. It is deliberately separate from
	// the context: finishing a download is a state transition into seeding,
	// not a shutdown.
	completed    atomic.Bool
	completedAt  atomic.Int64
	downloadDone chan struct{}
	completeOnce sync.Once

	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
}

func NewTorrent(tf *torrentfile.TorrentFile, outDir string) (*Torrent, error) {
	return NewTorrentWithOptions(tf, outDir, Options{})
}

func NewTorrentWithOptions(tf *torrentfile.TorrentFile, outDir string, opts Options) (*Torrent, error) {
	writer, err := p2p.NewMultiFileWriter(outDir, tf)
	if err != nil {
		return nil, err
	}

	maxPeers := opts.MaxPeers
	if maxPeers <= 0 {
		maxPeers = DefaultMaxPeers
	}

	ctx, cancel := context.WithCancel(context.Background())
	totalPieces := len(tf.PieceHashes)

	return &Torrent{
		TF:           tf,
		Writer:       writer,
		status:       StatusStopped,
		totalPieces:  totalPieces,
		bitfield:     p2p.NewSafeBitfield(totalPieces),
		swarm:        newSwarm(maxPeers),
		maxPeers:     maxPeers,
		opts:         opts,
		workQueue:    make(chan *p2p.PieceWork, totalPieces),
		results:      make(chan *p2p.PieceResult, 100),
		peerChan:     make(chan torrentfile.Peer, 500),
		events:       make(chan string, 256),
		downloadDone: make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}, nil
}

// Events reports what the engine is doing. Lower layers used to fmt.Printf
// straight to stdout, which scribbled over the TUI's alt-screen.
func (t *Torrent) Events() <-chan string { return t.events }

// logf never blocks: if the UI is not draining events, the message is dropped
// rather than stalling the download.
func (t *Torrent) logf(format string, args ...interface{}) {
	select {
	case t.events <- fmt.Sprintf(format, args...):
	default:
	}
}

func (t *Torrent) setStatus(s Status) {
	t.mu.Lock()
	t.status = s
	t.mu.Unlock()
}

func (t *Torrent) pieceLength(i int) int {
	if i == t.totalPieces-1 {
		if remainder := t.TF.Length % t.TF.PieceLength; remainder != 0 {
			return remainder
		}
	}
	return t.TF.PieceLength
}

// AddPeer injects a peer address to connect to, bypassing tracker and DHT
// discovery. Used by tests, and the natural entry point for PEX later.
func (t *Torrent) AddPeer(p torrentfile.Peer) {
	select {
	case t.peerChan <- p:
	case <-t.ctx.Done():
	default:
	}
}

// VerifyExistingState re-hashes whatever is already on disk so an interrupted
// download resumes instead of starting over.
func (t *Torrent) VerifyExistingState() {
	t.setStatus(StatusVerifying)
	t.logf("Scanning existing files for %s...", t.TF.Name)

	done := 0
	var bytes int64

	for i, expectedHash := range t.TF.PieceHashes {
		select {
		case <-t.ctx.Done():
			return
		default:
		}

		length := t.pieceLength(i)
		if data, err := t.Writer.ReadPiece(i, length); err == nil {
			if sha1.Sum(data) == expectedHash {
				t.bitfield.Set(i)
				done++
				bytes += int64(length)
			}
		}

		t.mu.Lock()
		t.verified = i + 1
		t.mu.Unlock()
	}

	t.mu.Lock()
	t.piecesDone = done
	t.downloaded = bytes
	t.mu.Unlock()

	t.logf("Resume: %d/%d pieces already valid", done, t.totalPieces)
}

func (t *Torrent) GetStats() TorrentStats {
	uploaded := t.swarm.uploaded()

	t.mu.RLock()
	defer t.mu.RUnlock()

	total := int64(t.TF.Length)

	progress := 0.0
	if t.status == StatusVerifying {
		if t.totalPieces > 0 {
			progress = float64(t.verified) / float64(t.totalPieces) * 100
		}
	} else if t.totalPieces > 0 {
		progress = float64(t.piecesDone) / float64(t.totalPieces) * 100
	}

	// Rolling rates over the recent window rather than an average over the
	// whole session, which barely moved once a torrent had been running a while.
	var downRate, upRate float64
	if len(t.samples) >= 2 {
		first, last := t.samples[0], t.samples[len(t.samples)-1]
		if elapsed := last.at.Sub(first.at).Seconds(); elapsed > 0 {
			downRate = float64(last.down-first.down) / elapsed
			upRate = float64(last.up-first.up) / elapsed
		}
	}

	var eta time.Duration
	if downRate > 0 && t.downloaded < total {
		eta = time.Duration(float64(total-t.downloaded)/downRate) * time.Second
	}

	ratio := 0.0
	if t.downloaded > 0 {
		ratio = float64(uploaded) / float64(t.downloaded)
	}

	return TorrentStats{
		Name:          t.TF.Name,
		Progress:      progress,
		Status:        t.status,
		PeersActive:   t.swarm.count(),
		PeersUnchoked: t.swarm.unchokedCount(),
		SpeedBps:      downRate,
		UploadBps:     upRate,
		Downloaded:    t.downloaded,
		Uploaded:      uploaded,
		Ratio:         ratio,
		Total:         total,
		ETA:           eta,
	}
}

// Err returns the failure message when Status is StatusError.
func (t *Torrent) Err() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.errText
}

func (t *Torrent) fail(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)

	t.mu.Lock()
	t.status = StatusError
	t.errText = msg
	t.mu.Unlock()

	t.logf("Error: %s", msg)
	t.cancel()
}

func (t *Torrent) Start(peerID [20]byte, port uint16) {
	t.setStatus(StatusStarting)

	go func() {
		// Verification re-hashes every existing byte on disk. It used to run
		// synchronously inside Start, which the TUI calls from its update loop,
		// freezing the interface for the whole scan.
		t.VerifyExistingState()

		select {
		case <-t.ctx.Done():
			t.setStatus(StatusStopped)
			return
		default:
		}

		t.mu.Lock()
		t.lastPieceAt = time.Now()
		t.samples = []sample{{at: time.Now(), down: t.downloaded}}
		remaining := t.totalPieces - t.piecesDone
		t.mu.Unlock()

		// Everything the torrent needs runs regardless of whether we still have
		// pieces to fetch: a complete torrent goes straight to seeding rather
		// than returning immediately as it used to.
		t.listen(peerID, port)
		go t.announceLoop(peerID, port)
		go t.managePeers(peerID)
		go t.chokeLoop()
		go t.sampleLoop()

		if remaining == 0 {
			t.logf("All pieces already present")
			t.markComplete()
		} else {
			t.setStatus(StatusDownloading)
			t.fillWorkQueue()

			// DHT is only useful while we still need peers to download from.
			go dht.FindPeers(t.ctx, t.TF.InfoHash, t.peerChan, t.logf)

			t.collect()
		}

		t.seedUntilDone()
	}()
}

// fillWorkQueue enqueues the pieces we still need, in random order. (Rarest
// first would be better, but needs swarm-wide availability tracking.)
func (t *Torrent) fillWorkQueue() {
	var missing []int
	for i := 0; i < t.totalPieces; i++ {
		if !t.bitfield.Has(i) {
			missing = append(missing, i)
		}
	}

	rand.Shuffle(len(missing), func(i, j int) {
		missing[i], missing[j] = missing[j], missing[i]
	})

	for _, i := range missing {
		t.workQueue <- &p2p.PieceWork{
			Index:  i,
			Hash:   t.TF.PieceHashes[i],
			Length: t.pieceLength(i),
		}
	}
}

// markComplete transitions the torrent from downloading to seeding.
func (t *Torrent) markComplete() {
	t.completeOnce.Do(func() {
		t.completed.Store(true)
		t.completedAt.Store(time.Now().UnixNano())
		t.setStatus(StatusSeeding)
		close(t.downloadDone)
	})
}

// seedUntilDone blocks while the torrent seeds, enforcing the stop conditions.
// It returns when the torrent is stopped, at which point the files are closed.
func (t *Torrent) seedUntilDone() {
	defer t.shutdown()

	t.mu.RLock()
	status := t.status
	t.mu.RUnlock()
	if status == StatusError || status == StatusStopped {
		return
	}

	if t.opts.SeedRatio <= 0 && t.opts.SeedTime <= 0 {
		<-t.ctx.Done()
		t.setStatus(StatusStopped)
		return
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			t.setStatus(StatusStopped)
			return

		case <-ticker.C:
			stats := t.GetStats()

			if t.opts.SeedRatio > 0 && stats.Ratio >= t.opts.SeedRatio {
				t.logf("Seed ratio %.2f reached, stopping", stats.Ratio)
				t.stopSeeding()
				return
			}

			if t.opts.SeedTime > 0 {
				since := time.Since(time.Unix(0, t.completedAt.Load()))
				if since >= t.opts.SeedTime {
					t.logf("Seeded for %s, stopping", t.opts.SeedTime)
					t.stopSeeding()
					return
				}
			}
		}
	}
}

// stopSeeding ends seeding because a limit was reached. Returning straight out
// of the loop skips the ctx.Done branch, so the status has to be set here.
func (t *Torrent) stopSeeding() {
	t.setStatus(StatusStopped)
	t.Stop()
}

// shutdown waits for peer sessions to finish, then closes the files. Closing
// used to be a defer in Start, so it fired the moment the download finished and
// made seeding impossible.
func (t *Torrent) shutdown() {
	t.cancel()
	t.swarm.closeAll()
	t.sessions.Wait()
	t.Writer.Close()
}

// sampleLoop keeps the rolling transfer-rate window fed even when no pieces are
// landing, which is the normal case while seeding.
func (t *Torrent) sampleLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			t.recordSample()
		}
	}
}

func (t *Torrent) recordSample() {
	uploaded := t.swarm.uploaded()
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	t.samples = append(t.samples, sample{at: now, down: t.downloaded, up: uploaded})

	cutoff := now.Add(-rateWindow)
	for len(t.samples) > 2 && t.samples[0].at.Before(cutoff) {
		t.samples = t.samples[1:]
	}
}

// chokeLoop runs the choke algorithm.
func (t *Torrent) chokeLoop() {
	ticker := time.NewTicker(chokeInterval * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			t.swarm.chokeRound(t.completed.Load())
		case <-t.swarm.nudge:
			t.swarm.chokeRound(t.completed.Load())
		}
	}
}

// announceLoop announces to every tracker, then re-announces at the interval the
// trackers ask for. Previously each tracker was contacted exactly once, so on a
// long download the peer supply went stale and never recovered.
func (t *Torrent) announceLoop(peerID [20]byte, port uint16) {
	event := torrentfile.EventStarted

	for {
		interval := t.announceRound(peerID, port, event)
		event = torrentfile.EventNone

		select {
		case <-t.ctx.Done():
			// Best-effort final announce so trackers record the outcome and
			// drop us promptly.
			t.announceRound(peerID, port, torrentfile.EventStopped)
			return

		case <-t.downloadDone:
			// Completion is announced once, then we carry on seeding.
			t.announceRound(peerID, port, torrentfile.EventCompleted)

		case <-time.After(interval):
		}
	}
}

// announceRound contacts every tracker in parallel and returns how long to wait
// before the next round.
func (t *Torrent) announceRound(
	peerID [20]byte,
	port uint16,
	event torrentfile.AnnounceEvent,
) time.Duration {
	trackers := t.TF.Trackers
	if len(trackers) == 0 {
		return minAnnounceInterval
	}

	t.mu.RLock()
	downloaded := t.downloaded
	t.mu.RUnlock()

	req := torrentfile.AnnounceReq{
		PeerID:     peerID,
		Port:       port,
		Downloaded: downloaded,
		Uploaded:   t.swarm.uploaded(),
		Left:       int64(t.TF.Length) - downloaded,
		Event:      event,
	}
	if req.Left < 0 {
		req.Left = 0
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		next = minAnnounceInterval
	)

	for _, trackerURL := range trackers {
		wg.Add(1)
		go func(trackerURL string) {
			defer wg.Done()

			// AnnounceTo dispatches on scheme. The engine used to call the UDP
			// path for every tracker, so http:// entries always timed out.
			resp, err := t.TF.AnnounceTo(trackerURL, req)
			if err != nil {
				t.logf("Tracker %s: %v", trackerURL, err)
				return
			}

			t.logf("Tracker %s: %d peers", trackerURL, len(resp.Peers))

			if resp.Interval > 0 {
				mu.Lock()
				if resp.Interval > next {
					next = resp.Interval
				}
				mu.Unlock()
			}

			for _, p := range resp.Peers {
				select {
				case t.peerChan <- p:
				case <-t.ctx.Done():
					return
				default:
				}
			}
		}(trackerURL)
	}

	wg.Wait()

	if next < minAnnounceInterval {
		next = minAnnounceInterval
	}
	return next
}

// managePeers dials each distinct peer we hear about, up to the peer limit.
func (t *Torrent) managePeers(peerID [20]byte) {
	seen := make(map[string]bool)

	for {
		var peer torrentfile.Peer
		select {
		case <-t.ctx.Done():
			return
		case p, ok := <-t.peerChan:
			if !ok {
				return
			}
			peer = p
		}

		addr := peer.String()
		if seen[addr] || t.swarm.has(addr) {
			continue
		}
		seen[addr] = true

		if t.swarm.count() >= t.maxPeers {
			continue
		}

		go t.dialPeer(peer, peerID)
	}
}

func (t *Torrent) dialPeer(peer torrentfile.Peer, peerID [20]byte) {
	session, err := p2p.Dial(
		t.ctx, peer, t.TF, peerID, t.bitfield, t.Writer,
		t.workQueue, t.results, t.logf, t.opts.Encryption,
	)
	if err != nil {
		return
	}

	session.SetOnInterest(t.swarm.requestChokeRound)

	if !t.swarm.add(session) {
		session.Close()
		return
	}
	defer t.swarm.remove(session)

	t.sessions.Add(1)
	defer t.sessions.Done()

	session.Run()
}

// collect writes completed pieces and watches for stalls. It returns when the
// download is finished or the torrent is stopped.
func (t *Torrent) collect() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		t.mu.RLock()
		done := t.piecesDone >= t.totalPieces
		t.mu.RUnlock()
		if done {
			break
		}

		select {
		case <-t.ctx.Done():
			t.setStatus(StatusStopped)
			return

		case <-ticker.C:
			// Nothing landing for a while means the swarm has gone quiet.
			t.mu.Lock()
			stalled := time.Since(t.lastPieceAt) > stallAfter
			if stalled && t.status == StatusDownloading {
				t.status = StatusStalled
				// Queued pieces are waiting for a peer that has them; the
				// rest are claimed by a session that is not delivering.
				// Telling the two apart is most of diagnosing a stall.
				remaining := t.totalPieces - t.piecesDone
				queued := len(t.workQueue)
				t.logf("No pieces for %s - %d left: %d queued, %d in flight",
					stallAfter, remaining, queued, remaining-queued)
			}
			t.mu.Unlock()

		case res := <-t.results:
			if err := t.Writer.WritePiece(res.Index, res.Buf); err != nil {
				// A dropped write used to be silent, so the piece was lost and
				// the download sat at 99% forever.
				t.fail("writing piece %d: %v", res.Index, err)
				return
			}

			t.bitfield.Set(res.Index)

			t.mu.Lock()
			t.piecesDone++
			t.downloaded += int64(len(res.Buf))
			t.lastPieceAt = time.Now()
			if t.status == StatusStalled {
				t.status = StatusDownloading
			}
			t.mu.Unlock()

			// Let every connected peer know, so they can ask us for it.
			t.swarm.broadcastHave(res.Index)
			t.recordSample()
		}
	}

	t.logf("Download complete: %s", t.TF.Name)
	t.markComplete()
}

// Stop halts the torrent. Safe to call more than once; it used to close a
// channel unconditionally and panic on the second call.
func (t *Torrent) Stop() {
	t.stopOnce.Do(func() {
		t.cancel()
	})
}
