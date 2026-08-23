package engine

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/bencode"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// buildTorrent synthesises a multi-file torrent plus its payload, so the seeding
// tests need neither a fixture with real data nor a network.
func buildTorrent(t *testing.T, pieceLength int, fileLengths ...int) (*torrentfile.TorrentFile, []byte) {
	t.Helper()

	total := 0
	for _, n := range fileLengths {
		total += n
	}

	data := make([]byte, total)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}

	var hashes []byte
	for off := 0; off < total; off += pieceLength {
		end := off + pieceLength
		if end > total {
			end = total
		}
		sum := sha1.Sum(data[off:end])
		hashes = append(hashes, sum[:]...)
	}

	files := make([]bencode.Value, 0, len(fileLengths))
	for i, n := range fileLengths {
		files = append(files, map[string]bencode.Value{
			"length": int64(n),
			"path":   []bencode.Value{"part" + string(rune('a'+i)) + ".bin"},
		})
	}

	encoded, err := bencode.Encode(map[string]bencode.Value{
		"info": map[string]bencode.Value{
			"name":         "synthetic",
			"piece length": int64(pieceLength),
			"pieces":       string(hashes),
			"files":        files,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "synthetic.torrent")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}

	tf, err := torrentfile.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return tf, data
}

// writePayload lays the torrent's bytes out on disk the way a completed
// download would, so VerifyExistingState marks the torrent complete.
func writePayload(t *testing.T, dir string, tf *torrentfile.TorrentFile, data []byte) {
	t.Helper()

	base := filepath.Join(dir, tf.Name)
	offset := 0
	for _, f := range tf.Files {
		full := filepath.Join(append([]string{base}, f.Path...)...)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data[offset:offset+f.Length], 0644); err != nil {
			t.Fatal(err)
		}
		offset += f.Length
	}
}

func peerID(t *testing.T, tag byte) [20]byte {
	t.Helper()
	var id [20]byte
	copy(id[:], "-GT0001-")
	if _, err := rand.Read(id[8:]); err != nil {
		t.Fatal(err)
	}
	id[19] = tag
	return id
}

func drainEvents(t *testing.T, tor *Torrent, label string) {
	go func() {
		for msg := range tor.Events() {
			t.Logf("%s: %s", label, msg)
		}
	}()
}

// TestSeedToLeech is the end-to-end proof that uploading works: one instance
// holds the complete data and serves it, another starts empty and fetches the
// whole torrent from it over loopback. It exercises the listener, the inbound
// handshake, the choker, request validation, block reads and piece assembly,
// with no tracker, no DHT and no internet.
func TestSeedToLeech(t *testing.T) {
	// Several pieces, and files sized so pieces straddle boundaries.
	tf, data := buildTorrent(t, 16384, 40000, 60000, 31072)

	seedDir := t.TempDir()
	leechDir := t.TempDir()
	writePayload(t, seedDir, tf, data)

	// --- seeder: already complete, so it goes straight to seeding ---
	seeder, err := NewTorrent(tf, seedDir)
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(t, seeder, "seeder")
	defer seeder.Stop()

	seeder.Start(peerID(t, 'S'), 0) // port 0: let the OS choose

	var addr net.Addr
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if addr = seeder.ListenAddr(); addr != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if addr == nil {
		t.Fatal("seeder never started listening")
	}

	// Wait for it to finish verifying and enter the seeding state.
	if !waitForStatus(seeder, StatusSeeding, 15*time.Second) {
		t.Fatalf("seeder did not reach seeding, got %s (%.1f%%)",
			seeder.GetStats().Status, seeder.GetStats().Progress)
	}

	// --- leecher: empty directory, no trackers, pointed straight at the seeder ---
	leecher, err := NewTorrent(tf, leechDir)
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(t, leecher, "leecher")
	defer leecher.Stop()

	leecher.Start(peerID(t, 'L'), 0)

	tcpAddr := addr.(*net.TCPAddr)
	// Give the leecher a moment to finish verifying before injecting the peer,
	// since AddPeer is non-blocking.
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 20; i++ {
		leecher.AddPeer(torrentfile.Peer{IP: net.IPv4(127, 0, 0, 1), Port: uint16(tcpAddr.Port)})
		if leecher.GetStats().PeersActive > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !waitForStatus(leecher, StatusSeeding, 60*time.Second) {
		s := leecher.GetStats()
		t.Fatalf("leecher did not complete: %.2f%% (%s), peers=%d", s.Progress, s.Status, s.PeersActive)
	}

	// The transfer must have actually gone through the seeder.
	if up := seeder.GetStats().Uploaded; up < int64(tf.Length) {
		t.Errorf("seeder uploaded %d bytes, expected at least the torrent size %d", up, tf.Length)
	}

	// And the bytes on disk must match exactly.
	leecher.Stop()
	time.Sleep(500 * time.Millisecond)

	offset := 0
	for _, f := range tf.Files {
		full := filepath.Join(append([]string{leechDir, tf.Name}, f.Path...)...)
		got, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("reading %s: %v", filepath.Join(f.Path...), err)
		}
		if !bytes.Equal(got, data[offset:offset+f.Length]) {
			t.Errorf("%s: content does not match the original", filepath.Join(f.Path...))
		}
		offset += f.Length
	}
}

// A peer asking for a torrent we do not serve must be refused.
func TestListenerRejectsUnknownInfoHash(t *testing.T) {
	tf, data := buildTorrent(t, 16384, 20000)

	dir := t.TempDir()
	writePayload(t, dir, tf, data)

	tor, err := NewTorrent(tf, dir)
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(t, tor, "seeder")
	defer tor.Stop()

	tor.Start(peerID(t, 'S'), 0)

	var addr net.Addr
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if addr = tor.ListenAddr(); addr != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if addr == nil {
		t.Fatal("never started listening")
	}

	conn, err := net.DialTimeout("tcp", addr.String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// A handshake for a completely different torrent.
	var wrong, id [20]byte
	copy(wrong[:], "wrongwrongwrongwrong")
	copy(id[:], "-GT0001-xxxxxxxxxxxx")

	handshake := make([]byte, 68)
	handshake[0] = 19
	copy(handshake[1:20], "BitTorrent protocol")
	copy(handshake[28:48], wrong[:])
	copy(handshake[48:68], id[:])

	if _, err := conn.Write(handshake); err != nil {
		t.Fatal(err)
	}

	// We must not answer; the peer should just see the connection close.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 68)
	if n, err := conn.Read(buf); err == nil && n > 0 {
		t.Errorf("server replied to a handshake for an unknown info hash (%d bytes)", n)
	}
}

func waitForStatus(tor *Torrent, want Status, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if tor.GetStats().Status == want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// A seed-time limit must stop the torrent on its own.
func TestSeedTimeLimit(t *testing.T) {
	tf, data := buildTorrent(t, 16384, 20000)

	dir := t.TempDir()
	writePayload(t, dir, tf, data)

	tor, err := NewTorrentWithLimits(tf, dir, Limits{SeedTime: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(t, tor, "seeder")
	defer tor.Stop()

	tor.Start(peerID(t, 'S'), 0)

	if !waitForStatus(tor, StatusSeeding, 10*time.Second) {
		t.Fatalf("never started seeding, got %s", tor.GetStats().Status)
	}

	if !waitForStatus(tor, StatusStopped, 20*time.Second) {
		t.Fatalf("seed-time limit did not stop the torrent, status %s", tor.GetStats().Status)
	}
}

// Files must stay open and readable while seeding. Writer.Close used to be a
// defer in Start, so it fired the instant the download finished.
func TestFilesStayOpenWhileSeeding(t *testing.T) {
	tf, data := buildTorrent(t, 16384, 30000)

	dir := t.TempDir()
	writePayload(t, dir, tf, data)

	tor, err := NewTorrent(tf, dir)
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(t, tor, "seeder")
	defer tor.Stop()

	tor.Start(peerID(t, 'S'), 0)

	if !waitForStatus(tor, StatusSeeding, 10*time.Second) {
		t.Fatalf("never started seeding, got %s", tor.GetStats().Status)
	}

	// Give the old teardown path a chance to fire if it were still there.
	time.Sleep(500 * time.Millisecond)

	// Serving a peer request goes through exactly this call.
	block, err := tor.Writer.ReadBlock(0, 0, 1024)
	if err != nil {
		t.Fatalf("cannot read a block while seeding: %v", err)
	}
	if !bytes.Equal(block, data[:1024]) {
		t.Error("block read while seeding returned the wrong bytes")
	}
}
