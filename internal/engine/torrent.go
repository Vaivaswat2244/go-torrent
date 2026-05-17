package engine

import (
	"crypto/sha1"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/dht"
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

	sessionPieces int       // Pieces downloaded *this* time
	startTime     time.Time // When we started

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

	speed := 0.0
	if t.sessionPieces > 0 {
		elapsed := time.Since(t.startTime).Seconds()
		if elapsed > 0 {
			bytesDownloaded := float64(t.sessionPieces * t.TF.PieceLength)
			speed = bytesDownloaded / elapsed / 1024 / 1024 // Convert to MB/s
		}
	}

	return TorrentStats{
		Name:        t.TF.Name,
		Progress:    progress,
		Status:      t.status,
		PeersActive: t.activePeers,
		SpeedMBps:   speed,
	}
}

// Start begins the download in the background
// Start begins the download in the background
func (t *Torrent) Start(peerID [20]byte, port uint16) {
	t.mu.Lock()
	t.status = StatusStarting
	t.startTime = time.Now()
	t.mu.Unlock()

	// 1. Verify what we already have on disk
	t.VerifyExistingState()

	// Launch the main orchestrator in a background goroutine
	go func() {
		defer t.Writer.Close()

		t.mu.Lock()
		t.status = StatusDownloading
		t.mu.Unlock()

		// 2. Setup Channels
		peerChan := make(chan torrentfile.Peer, 100) // Buffer for incoming peers
		workQueue := make(chan *p2p.PieceWork, t.totalPieces)
		results := make(chan *p2p.PieceResult)

		// 3. Populate Work Queue (Shuffled for Rarest-First emulation)
		var missingPieces []int
		for i := 0; i < t.totalPieces; i++ {
			if !t.bitfield.HasPiece(i) {
				missingPieces = append(missingPieces, i)
			}
		}

		rand.Seed(time.Now().UnixNano())
		rand.Shuffle(len(missingPieces), func(i, j int) {
			missingPieces[i], missingPieces[j] = missingPieces[j], missingPieces[i]
		})

		for _, i := range missingPieces {
			hash := t.TF.PieceHashes[i]
			length := t.TF.PieceLength
			if i == t.totalPieces-1 {
				remainder := t.TF.Length % t.TF.PieceLength
				if remainder != 0 {
					length = remainder
				}
			}
			workQueue <- &p2p.PieceWork{Index: i, Hash: hash, Length: length}
		}

		// 4. 🚀 LAUNCH THE TRACKER (Background)
		go func() {
			peers, err := t.TF.RequestPeers(peerID, port)
			if err == nil {
				for _, p := range peers {
					peerChan <- p
				}
			}
		}()

		// 5. 🚀 LAUNCH THE DHT CRAWLER (Background)
		go dht.FindPeers(t.TF.InfoHash, peerChan)

		// 6. The Peer Manager (Listens for peers and launches workers dynamically)
		go func() {
			seenPeers := make(map[string]bool)
			for peer := range peerChan {
				addr := peer.String()

				// Deduplicate: Only launch a worker if we haven't seen this IP yet
				if !seenPeers[addr] {
					seenPeers[addr] = true

					t.mu.Lock()
					t.activePeers++
					t.mu.Unlock()

					// Launch a new worker for this newly discovered peer!
					go p2p.Worker(peer, t.TF, peerID, t.bitfield, workQueue, results)
				}
			}
		}()

		// 7. Collect Results
		for t.piecesDone < t.totalPieces {
			select {
			case <-t.stopChan:
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
					t.sessionPieces++

					// Update our bitfield so we know we have it
					t.bitfield.SetPiece(res.Index)
					t.mu.Unlock()
				}
			}
		}

		// 8. Finished!
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
