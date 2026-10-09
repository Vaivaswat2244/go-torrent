package engine

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/mse"
	"github.com/Vaivaswat2244/go-torrent/internal/peers"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// These tests join a real public swarm. They are skipped unless
// GOTORRENT_LIVE=1 is set, so the default `go test ./...` stays offline.
//
//	GOTORRENT_LIVE=1 go test -race ./internal/engine/ -v -timeout 10m
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("GOTORRENT_LIVE") != "1" {
		t.Skip("set GOTORRENT_LIVE=1 to run tests that hit the network")
	}
}

func testPeerID() [20]byte {
	var id [20]byte
	copy(id[:], "-GT0001-")
	rand.Read(id[8:])
	return id
}

// Sintel is a Blender open movie: small, multi-file, and well seeded, so it
// exercises the piece writer's file-boundary math against real data.
const liveFixture = "../../testdata/sintel.torrent"

func TestLiveDownload(t *testing.T) {
	requireLive(t)

	tf, err := torrentfile.Open(liveFixture)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	tor, err := NewTorrent(tf, dir)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		for msg := range tor.Events() {
			t.Logf("event: %s", msg)
		}
	}()

	tor.Start(testPeerID(), 6881)

	deadline := time.After(5 * time.Minute)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	var last TorrentStats
loop:
	for {
		select {
		case <-deadline:
			break loop
		case <-ticker.C:
			last = tor.GetStats()
			t.Logf("%-12s %6.2f%%  down %.2f MB/s  up %.2f MB/s  peers=%d (%d served)  uploaded %d  eta=%s",
				last.Status, last.Progress,
				last.SpeedBps/1024/1024, last.UploadBps/1024/1024,
				last.PeersActive, last.PeersUnchoked, last.Uploaded, last.ETA)

			if last.Status == StatusSeeding {
				break loop
			}
			if last.Status == StatusError {
				t.Fatalf("engine error: %s", tor.Err())
			}
		}
	}

	// While the torrent is still running, completion must have left it seeding
	// rather than torn down: files open, listener up. This has to be checked
	// before Stop, which is what closes the files.
	if last.Status == StatusSeeding {
		if _, err := tor.Writer.ReadBlock(0, 0, 1024); err != nil {
			t.Errorf("cannot serve a block after completion: %v", err)
		}
		if tor.ListenAddr() == nil {
			t.Error("no listener while seeding")
		}
	}

	tor.Stop()
	time.Sleep(500 * time.Millisecond)

	// Every file must exist at its expected path and preallocated size.
	for _, f := range tf.Files {
		full := filepath.Join(append([]string{dir, tf.Name}, f.Path...)...)
		st, err := os.Stat(full)
		if err != nil {
			t.Errorf("missing %s: %v", filepath.Join(f.Path...), err)
			continue
		}
		if st.Size() != int64(f.Length) {
			t.Errorf("%s: size %d, want %d", filepath.Join(f.Path...), st.Size(), f.Length)
		}
	}

	if last.Progress == 0 {
		t.Skip("no pieces downloaded - swarm unreachable from here")
	}
	if last.Status != StatusSeeding {
		t.Errorf("did not finish within the budget: %.2f%% (%s)", last.Progress, last.Status)
	}
}

// Downloads part of a torrent, stops, then verifies a fresh Torrent over the
// same directory recognises the existing data instead of re-fetching it.
func TestLiveResume(t *testing.T) {
	requireLive(t)

	tf, err := torrentfile.Open(liveFixture)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	peerID := testPeerID()

	first, err := NewTorrent(tf, dir)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range first.Events() {
		}
	}()

	first.Start(peerID, 6881)
	time.Sleep(45 * time.Second)

	partial := first.GetStats()
	first.Stop()
	time.Sleep(time.Second)

	t.Logf("first run reached %.2f%%", partial.Progress)
	if partial.Progress == 0 {
		t.Skip("no pieces downloaded - swarm unreachable from here")
	}

	second, err := NewTorrent(tf, dir)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range second.Events() {
		}
	}()
	defer second.Stop()

	start := time.Now()
	second.VerifyExistingState()
	t.Logf("resume scan took %s", time.Since(start))

	resumed := second.GetStats()
	if resumed.Progress < partial.Progress*0.9 {
		t.Errorf("resume recovered %.2f%%, expected close to %.2f%%",
			resumed.Progress, partial.Progress)
	}
	if resumed.Downloaded == 0 {
		t.Error("resume reported zero downloaded bytes")
	}
}

// TestLiveHandshakes reports how many real peers complete a plain handshake
// versus an encrypted one. It doubles as a network diagnostic: on a network
// that resets BitTorrent, plain succeeds for nobody while encrypted still does.
//
//	GOTORRENT_LIVE=1 go test ./internal/engine/ -run TestLiveHandshakes -v
func TestLiveHandshakes(t *testing.T) {
	requireLive(t)

	id := testPeerID()

	for _, name := range []string{"sintel", "big-buck-bunny", "tears-of-steel", "cosmos-laundromat"} {
		tf, err := torrentfile.Open("../../testdata/" + name + ".torrent")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}

		// Gather peers from every tracker the torrent lists, in parallel: a
		// dead UDP tracker costs ~35s of retries on its own.
		addrs := map[string]bool{}
		var amu sync.Mutex
		var twg sync.WaitGroup
		for _, tr := range tf.Trackers {
			twg.Add(1)
			go func(tr string) {
				defer twg.Done()
				resp, err := tf.AnnounceTo(tr, torrentfile.AnnounceReq{
					PeerID: id, Port: 6881, Left: int64(tf.Length), Event: torrentfile.EventStarted,
				})
				if err != nil {
					return
				}
				amu.Lock()
				defer amu.Unlock()
				for _, p := range resp.Peers {
					addrs[p.String()] = true
				}
			}(tr)
		}
		twg.Wait()
		if len(addrs) == 0 {
			t.Logf("%-18s no peers from any tracker", name)
			continue
		}

		count := func(policy mse.Policy) int {
			var ok int64
			var wg sync.WaitGroup
			sem := make(chan struct{}, 40)
			for addr := range addrs {
				wg.Add(1)
				go func(addr string) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()

					c, err := peers.Connect(ctx, addr, tf.InfoHash, id, policy)
					if err != nil {
						return
					}
					c.Conn.Close()
					atomic.AddInt64(&ok, 1)
				}(addr)
			}
			wg.Wait()
			return int(ok)
		}

		plain := count(mse.PolicyDisable)
		encrypted := count(mse.PolicyRequire)

		t.Logf("%-18s peers=%-4d  plain handshakes=%-4d  encrypted handshakes=%d",
			name, len(addrs), plain, encrypted)

		// Real clients overwhelmingly support MSE. If plain works but not a
		// single encrypted handshake does, the fault is ours.
		if plain >= 5 && encrypted == 0 {
			t.Errorf("%s: %d peers accepted plain but none accepted encryption", name, plain)
		}
	}
}
