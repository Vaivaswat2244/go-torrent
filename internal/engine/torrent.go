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

type TorrentStats struct {
	Name        string
	Progress    float64
	SpeedMBps   float64
	Status      Status
	PeersActive int
}

type Torrent struct {
	TF     *torrentfile.TorrentFile
	Writer *p2p.MultiFileWriter

	bitfield p2p.Bitfield

	mu             sync.RWMutex
	status         Status
	piecesDone     int
	totalPieces    int
	bytesSinceLast time.Time
	activePeers    int

	sessionPieces int
	startTime     time.Time

	stopChan chan struct{}
}

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
	t.bitfield = make(p2p.Bitfield, int(math.Ceil(float64(t.totalPieces)/8.0)))

	fmt.Printf("🔍 Scanning existing files for %s...\n", t.TF.Name)

	for i, expectedHash := range t.TF.PieceHashes {
		length := t.TF.PieceLength
		if i == t.totalPieces-1 {
			remainder := t.TF.Length % t.TF.PieceLength
			if remainder != 0 {
				length = remainder
			}
		}

		data, err := t.Writer.ReadPiece(i, length)
		if err == nil {
			actualHash := sha1.Sum(data)
			if actualHash == expectedHash {
				t.bitfield.SetPiece(i)
				t.piecesDone++
			}
		}
	}

	fmt.Printf("✅ Resume State: Found %d/%d valid pieces.\n", t.piecesDone, t.totalPieces)
}

func (t *Torrent) GetStats() TorrentStats {
	t.mu.RLock()
	defer t.mu.RUnlock()

	progress := 0.0
	if t.totalPieces > 0 {
		progress = float64(t.piecesDone) / float64(t.totalPieces) * 100
	}

	speed := 0.0
	if t.sessionPieces > 0 {
		elapsed := time.Since(t.startTime).Seconds()
		if elapsed > 0 {
			bytesDownloaded := float64(t.sessionPieces * t.TF.PieceLength)
			speed = bytesDownloaded / elapsed / 1024 / 1024
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

func (t *Torrent) Start(peerID [20]byte, port uint16) {
	t.mu.Lock()
	t.status = StatusStarting
	t.startTime = time.Now()
	t.mu.Unlock()

	t.VerifyExistingState()

	go func() {
		defer t.Writer.Close()

		t.mu.Lock()
		t.status = StatusDownloading
		t.mu.Unlock()

		peerChan := make(chan torrentfile.Peer, 500)
		workQueue := make(chan *p2p.PieceWork, t.totalPieces)
		results := make(chan *p2p.PieceResult, 100)

		// Populate work queue
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

		// Launch tracker
		go func() {
			peers, err := t.TF.RequestPeers(peerID, port)
			if err == nil {
				for _, p := range peers {
					select {
					case peerChan <- p:
					default:
					}
				}
			}
		}()

		// Launch DHT
		go dht.FindPeers(t.TF.InfoHash, peerChan)

		// Peer manager — launches workers, tracks active count, retries dead workers
		go func() {
			seenPeers := make(map[string]bool)
			for peer := range peerChan {
				addr := peer.String()
				if seenPeers[addr] {
					continue
				}
				seenPeers[addr] = true

				t.mu.Lock()
				t.activePeers++
				t.mu.Unlock()

				go func(p torrentfile.Peer) {
					defer func() {
						t.mu.Lock()
						t.activePeers--
						t.mu.Unlock()
					}()

					// Retry peer up to 3 times before giving up
					for i := 0; i < 3; i++ {
						p2p.Worker(p, t.TF, peerID, t.bitfield, workQueue, results)
						// If work queue is empty, we're done — no need to retry
						if len(workQueue) == 0 {
							return
						}
						time.Sleep(time.Duration(i+1) * 2 * time.Second)
					}
				}(peer)
			}
		}()

		// Collect results
		for t.piecesDone < t.totalPieces {
			select {
			case <-t.stopChan:
				t.mu.Lock()
				t.status = StatusStopped
				t.mu.Unlock()
				return
			case res := <-results:
				err := t.Writer.WritePiece(res.Index, res.Buf)
				if err == nil {
					t.mu.Lock()
					t.piecesDone++
					t.sessionPieces++
					t.bitfield.SetPiece(res.Index)
					t.mu.Unlock()
				}
			}
		}

		t.mu.Lock()
		t.status = StatusSeeding
		t.mu.Unlock()
		close(workQueue)
	}()
}

func (t *Torrent) Stop() {
	close(t.stopChan)
}
