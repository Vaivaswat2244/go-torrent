package engine

import (
	"crypto/sha1"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/p2p"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

type Status string

const (
	StatusStarting    Status = "Starting"
	StatusDownloading Status = "Downloading"
	StatusSeeding     Status = "Seeding"
	StatusError       Status = "Error"
	StatusStopped     Status = "Stopped"
)

// TorrentStats is the clean data package we hand to the UI
type TorrentStats struct {
	Name        string
	Progress    float64
	SpeedMBps   float64
	Status      Status
	PeersActive int
}

// Torrent is the actual engine running the download
type Torrent struct {
	TF     *torrentfile.TorrentFile
	Writer *p2p.MultiFileWriter

	bitfield p2p.Bitfield

	// State variables (protected by Mutex for concurrent UI access)
	mu             sync.RWMutex
	status         Status
	piecesDone     int
	totalPieces    int
	bytesSinceLast time.Time
	activePeers    int

	// Channels
	stopChan chan struct{}
}

// NewTorrent initializes the state
func NewTorrent(tf *torrentfile.TorrentFile, outDir string) (*Torrent, error) {
	writer, err := p2p.NewMultiFileWriter(outDir, tf)
	if err != nil {
		return nil, err
	}

	return &Torrent{
		TF:          tf,
		Writer:      writer,
		status:      StatusStopped,
		totalPieces: len(tf.PieceHashes),
		stopChan:    make(chan struct{}),
	}, nil
}

func (t *Torrent) VerifyExistingState() {
	// Initialize an empty bitfield of the correct size
	t.bitfield = make(p2p.Bitfield, int(math.Ceil(float64(t.totalPieces)/8.0)))

	fmt.Printf("🔍 Scanning existing files for %s...\n", t.TF.Name)

	for i, expectedHash := range t.TF.PieceHashes {
		// Calculate expected length (handling the last piece)
		length := t.TF.PieceLength
		if i == t.totalPieces-1 {
			remainder := t.TF.Length % t.TF.PieceLength
			if remainder != 0 {
				length = remainder
			}
		}

		// Try to read the piece
		data, err := t.Writer.ReadPiece(i, length)
		if err == nil {
			// Hash it and compare!
			actualHash := sha1.Sum(data)
			if actualHash == expectedHash {
				t.bitfield.SetPiece(i)
				t.piecesDone++
			}
		}
	}

	fmt.Printf("✅ Resume State: Found %d/%d valid pieces.\n", t.piecesDone, t.totalPieces)
}

// GetStats safely reads the current state for the UI
func (t *Torrent) GetStats() TorrentStats {
	t.mu.RLock()         // Lock for reading
	defer t.mu.RUnlock() // Unlock when function returns

	progress := 0.0
	if t.totalPieces > 0 {
		progress = float64(t.piecesDone) / float64(t.totalPieces) * 100
	}

	return TorrentStats{
		Name:        t.TF.Name,
		Progress:    progress,
		Status:      t.status,
		PeersActive: t.activePeers,
		// (We'll calculate instantaneous speed later, setting to 0 for now)
		SpeedMBps: 0.0,
	}
}

// Start begins the download in the background
func (t *Torrent) Start(peerID [20]byte, port uint16) {
	t.mu.Lock()
	t.status = StatusStarting
	t.mu.Unlock()

	t.VerifyExistingState()

	// Launch in a background goroutine so it doesn't block the UI
	go func() {
		defer t.Writer.Close()

		// 1. Get Peers (Phase 1)
		peers, err := t.TF.RequestPeers(peerID, port)
		if err != nil {
			t.mu.Lock()
			t.status = StatusError
			t.mu.Unlock()
			return
		}

		t.mu.Lock()
		t.status = StatusDownloading
		t.activePeers = len(peers)
		t.mu.Unlock()

		// 2. Setup Work Queue (Phase 3)
		workQueue := make(chan *p2p.PieceWork, t.totalPieces)
		results := make(chan *p2p.PieceResult)

		for i, hash := range t.TF.PieceHashes {

			if t.bitfield.HasPiece(i) {
				continue
			}

			length := t.TF.PieceLength
			if i == t.totalPieces-1 {
				remainder := t.TF.Length % t.TF.PieceLength
				if remainder != 0 {
					length = remainder
				}
			}
			workQueue <- &p2p.PieceWork{Index: i, Hash: hash, Length: length}
		}

		// 3. Launch Workers
		for _, peer := range peers {
			go p2p.Worker(peer, t.TF, peerID, t.bitfield, workQueue, results)
		}

		// 4. Collect Results safely
		for t.piecesDone < t.totalPieces {
			select {
			case <-t.stopChan:
				// UI asked us to stop
				t.mu.Lock()
				t.status = StatusStopped
				t.mu.Unlock()
				return
			case res := <-results:
				// Write to disk
				err := t.Writer.WritePiece(res.Index, res.Buf)
				if err == nil {
					t.mu.Lock()
					t.piecesDone++
					t.mu.Unlock()
				}
			}
		}

		// 5. Finished!
		t.mu.Lock()
		t.status = StatusSeeding
		t.mu.Unlock()
		close(workQueue)
	}()
}

// Stop allows the UI to halt the download
func (t *Torrent) Stop() {
	close(t.stopChan)
}
