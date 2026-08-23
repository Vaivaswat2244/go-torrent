package engine

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

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
			t.Logf("%-12s %6.2f%%  %.2f MB/s  peers=%d  eta=%s",
				last.Status, last.Progress, last.SpeedBps/1024/1024, last.PeersActive, last.ETA)

			if last.Status == StatusSeeding {
				break loop
			}
			if last.Status == StatusError {
				t.Fatalf("engine error: %s", tor.Err())
			}
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
