package engine

import (
	"context"
	"crypto/sha1"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/dht"
	"github.com/Vaivaswat2244/go-torrent/internal/p2p"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

type Status string

const (
	StatusStarting    Status = "Starting"
	StatusVerifying   Status = "Verifying"
	StatusDownloading Status = "Downloading"
	StatusStalled     Status = "Stalled"
	StatusSeeding     Status = "Complete"
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

	// speedWindow is the period the reported rate is averaged over.
	speedWindow = 15 * time.Second
)

type TorrentStats struct {
	Name        string
	Progress    float64
	SpeedBps    float64
	Status      Status
	PeersActive int
	Downloaded  int64
	Total       int64
	ETA         time.Duration
}

type sample struct {
	at    time.Time
	bytes int64
}

type Torrent struct {
	TF     *torrentfile.TorrentFile
	Writer *p2p.MultiFileWriter

	bitfield *p2p.SafeBitfield

	mu          sync.RWMutex
	status      Status
	piecesDone  int
	totalPieces int
	activePeers int
	downloaded  int64
	verified    int // pieces scanned so far, for the verifying screen
	lastPieceAt time.Time
	samples     []sample
	errText     string

	events chan string

	// completed distinguishes "finished downloading" from "user quit" when the
	// context is cancelled, so trackers get the right final event.
	completed atomic.Bool

	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
}

func NewTorrent(tf *torrentfile.TorrentFile, outDir string) (*Torrent, error) {
	writer, err := p2p.NewMultiFileWriter(outDir, tf)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Torrent{
		TF:          tf,
		Writer:      writer,
		status:      StatusStopped,
		totalPieces: len(tf.PieceHashes),
		bitfield:    p2p.NewSafeBitfield(len(tf.PieceHashes)),
		events:      make(chan string, 256),
		ctx:         ctx,
		cancel:      cancel,
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

	// Rolling rate over the recent window rather than an average over the whole
	// session, which barely moved once a download had been running a while.
	speed := 0.0
	if len(t.samples) >= 2 {
		first, last := t.samples[0], t.samples[len(t.samples)-1]
		if elapsed := last.at.Sub(first.at).Seconds(); elapsed > 0 {
			speed = float64(last.bytes-first.bytes) / elapsed
		}
	}

	var eta time.Duration
	if speed > 0 && t.downloaded < total {
		eta = time.Duration(float64(total-t.downloaded)/speed) * time.Second
	}

	return TorrentStats{
		Name:        t.TF.Name,
		Progress:    progress,
		Status:      t.status,
		PeersActive: t.activePeers,
		SpeedBps:    speed,
		Downloaded:  t.downloaded,
		Total:       total,
		ETA:         eta,
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
		defer t.Writer.Close()

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
		t.status = StatusDownloading
		t.lastPieceAt = time.Now()
		t.samples = []sample{{at: time.Now(), bytes: t.downloaded}}
		remaining := t.totalPieces - t.piecesDone
		t.mu.Unlock()

		if remaining == 0 {
			t.setStatus(StatusSeeding)
			t.logf("All pieces already present")
			return
		}

		peerChan := make(chan torrentfile.Peer, 500)
		workQueue := make(chan *p2p.PieceWork, t.totalPieces)
		results := make(chan *p2p.PieceResult, 100)

		// Populate the work queue in random order. (Rarest-first would be
		// better, but needs swarm-wide availability tracking.)
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
			workQueue <- &p2p.PieceWork{
				Index:  i,
				Hash:   t.TF.PieceHashes[i],
				Length: t.pieceLength(i),
			}
		}

		go t.announceLoop(peerID, port, peerChan)
		go dht.FindPeers(t.ctx, t.TF.InfoHash, peerChan, t.logf)
		go t.managePeers(peerID, peerChan, workQueue, results)

		t.collect(results, workQueue)
	}()
}

// announceLoop announces to every tracker, then re-announces at the interval the
// trackers ask for. Previously each tracker was contacted exactly once, so on a
// long download the peer supply went stale and never recovered.
func (t *Torrent) announceLoop(peerID [20]byte, port uint16, peerChan chan<- torrentfile.Peer) {
	event := torrentfile.EventStarted

	for {
		interval := t.announceRound(peerID, port, peerChan, event)
		event = torrentfile.EventNone

		select {
		case <-t.ctx.Done():
			// Best-effort final announce so trackers record the outcome and
			// drop us promptly.
			final := torrentfile.EventStopped
			if t.completed.Load() {
				final = torrentfile.EventCompleted
			}
			t.announceRound(peerID, port, nil, final)
			return
		case <-time.After(interval):
		}
	}
}

// announceRound contacts every tracker in parallel and returns how long to wait
// before the next round.
func (t *Torrent) announceRound(
	peerID [20]byte,
	port uint16,
	peerChan chan<- torrentfile.Peer,
	event torrentfile.AnnounceEvent,
) time.Duration {
	trackers := t.TF.Trackers
	if len(trackers) == 0 {
		return minAnnounceInterval
	}

	t.mu.RLock()
	req := torrentfile.AnnounceReq{
		PeerID:     peerID,
		Port:       port,
		Downloaded: t.downloaded,
		Uploaded:   0, // no upload path yet
		Left:       int64(t.TF.Length) - t.downloaded,
		Event:      event,
	}
	t.mu.RUnlock()
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
				if peerChan == nil {
					break
				}
				select {
				case peerChan <- p:
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

// managePeers dials each distinct peer we hear about and runs a worker against it.
func (t *Torrent) managePeers(
	peerID [20]byte,
	peerChan <-chan torrentfile.Peer,
	workQueue chan *p2p.PieceWork,
	results chan *p2p.PieceResult,
) {
	seen := make(map[string]bool)

	for {
		var peer torrentfile.Peer
		select {
		case <-t.ctx.Done():
			return
		case p, ok := <-peerChan:
			if !ok {
				return
			}
			peer = p
		}

		addr := peer.String()
		if seen[addr] {
			continue
		}
		seen[addr] = true

		go func(p torrentfile.Peer) {
			t.mu.Lock()
			t.activePeers++
			t.mu.Unlock()
			defer func() {
				t.mu.Lock()
				t.activePeers--
				t.mu.Unlock()
			}()

			for i := 0; i < 3; i++ {
				p2p.Worker(t.ctx, p, t.TF, peerID, t.bitfield, workQueue, results)

				select {
				case <-t.ctx.Done():
					return
				default:
				}
				if len(workQueue) == 0 {
					return
				}

				select {
				case <-t.ctx.Done():
					return
				case <-time.After(time.Duration(i+1) * 2 * time.Second):
				}
			}
		}(peer)
	}
}

// collect writes completed pieces and watches for stalls.
func (t *Torrent) collect(results chan *p2p.PieceResult, workQueue chan *p2p.PieceWork) {
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
				t.logf("No pieces for %s - still trying", stallAfter)
			}
			t.mu.Unlock()

		case res := <-results:
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
			t.samples = append(t.samples, sample{at: t.lastPieceAt, bytes: t.downloaded})
			cutoff := t.lastPieceAt.Add(-speedWindow)
			for len(t.samples) > 2 && t.samples[0].at.Before(cutoff) {
				t.samples = t.samples[1:]
			}
			t.mu.Unlock()
		}
	}

	t.completed.Store(true)
	t.setStatus(StatusSeeding)
	t.logf("Download complete: %s", t.TF.Name)

	// The work queue is deliberately never closed: workers may still be holding
	// a piece to re-queue, and a send on a closed channel would panic at the
	// exact moment the download succeeded. Cancelling stops them instead.
	t.cancel()
}

// Stop halts the torrent. Safe to call more than once; it used to close a
// channel unconditionally and panic on the second call.
func (t *Torrent) Stop() {
	t.stopOnce.Do(func() {
		t.cancel()
	})
}
