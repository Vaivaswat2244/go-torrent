package p2p

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// A layout where pieces deliberately straddle file boundaries: with a piece
// length of 16 and files of 10/20/5 bytes, piece 0 spans files 0 and 1, and
// piece 1 spans files 1 and 2.
func testTorrent() *torrentfile.TorrentFile {
	return &torrentfile.TorrentFile{
		Name:        "multi",
		PieceLength: 16,
		Length:      35,
		IsMultiFile: true,
		Files: []torrentfile.FileInfo{
			{Length: 10, Path: []string{"a.bin"}},
			{Length: 20, Path: []string{"sub", "b.bin"}},
			{Length: 5, Path: []string{"c.bin"}},
		},
	}
}

func payload(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i + 1)
	}
	return data
}

func TestMultiFileWriterSpansBoundaries(t *testing.T) {
	dir := t.TempDir()
	tf := testTorrent()
	data := payload(tf.Length)

	mw, err := NewMultiFileWriter(dir, tf)
	if err != nil {
		t.Fatal(err)
	}

	// Write every piece, including the short final one.
	for i, start := 0, 0; start < len(data); i, start = i+1, start+tf.PieceLength {
		end := start + tf.PieceLength
		if end > len(data) {
			end = len(data)
		}
		if err := mw.WritePiece(i, data[start:end]); err != nil {
			t.Fatalf("WritePiece(%d): %v", i, err)
		}
	}
	mw.Close()

	// Each file on disk must hold its own slice of the global byte stream.
	var offset int
	for _, f := range tf.Files {
		full := filepath.Join(append([]string{dir, tf.Name}, f.Path...)...)
		got, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("reading %s: %v", full, err)
		}
		want := data[offset : offset+f.Length]
		if !bytes.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", filepath.Join(f.Path...), got, want)
		}
		offset += f.Length
	}
}

func TestMultiFileWriterReadPieceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tf := testTorrent()
	data := payload(tf.Length)

	mw, err := NewMultiFileWriter(dir, tf)
	if err != nil {
		t.Fatal(err)
	}
	defer mw.Close()

	for i, start := 0, 0; start < len(data); i, start = i+1, start+tf.PieceLength {
		end := start + tf.PieceLength
		if end > len(data) {
			end = len(data)
		}
		if err := mw.WritePiece(i, data[start:end]); err != nil {
			t.Fatal(err)
		}
	}

	// ReadPiece backs resume verification, so it must return exactly what
	// WritePiece stored, including for the short final piece.
	for i, start := 0, 0; start < len(data); i, start = i+1, start+tf.PieceLength {
		end := start + tf.PieceLength
		if end > len(data) {
			end = len(data)
		}
		got, err := mw.ReadPiece(i, end-start)
		if err != nil {
			t.Fatalf("ReadPiece(%d): %v", i, err)
		}
		if !bytes.Equal(got, data[start:end]) {
			t.Errorf("piece %d: got %v, want %v", i, got, data[start:end])
		}
	}
}

func TestSingleFileTorrentHasNoContainingDir(t *testing.T) {
	dir := t.TempDir()
	tf := &torrentfile.TorrentFile{
		Name:        "solo.bin",
		PieceLength: 16,
		Length:      10,
		IsMultiFile: false,
		Files:       []torrentfile.FileInfo{{Length: 10, Path: []string{"solo.bin"}}},
	}

	mw, err := NewMultiFileWriter(dir, tf)
	if err != nil {
		t.Fatal(err)
	}
	if err := mw.WritePiece(0, payload(10)); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	if _, err := os.Stat(filepath.Join(dir, "solo.bin")); err != nil {
		t.Errorf("single-file torrent should write directly into the output dir: %v", err)
	}
}

// A multi-file torrent with exactly one file still needs its containing folder;
// this used to be inferred from len(Files) > 1 and got it wrong.
func TestSingleEntryMultiFileTorrentKeepsContainingDir(t *testing.T) {
	dir := t.TempDir()
	tf := &torrentfile.TorrentFile{
		Name:        "pack",
		PieceLength: 16,
		Length:      10,
		IsMultiFile: true,
		Files:       []torrentfile.FileInfo{{Length: 10, Path: []string{"only.bin"}}},
	}

	mw, err := NewMultiFileWriter(dir, tf)
	if err != nil {
		t.Fatal(err)
	}
	mw.Close()

	if _, err := os.Stat(filepath.Join(dir, "pack", "only.bin")); err != nil {
		t.Errorf("expected pack/only.bin: %v", err)
	}
}

// The writer opens files with O_CREATE, so it must reject an escaping path even
// if one somehow gets past the parser.
func TestMultiFileWriterRejectsEscapingPath(t *testing.T) {
	dir := t.TempDir()
	tf := &torrentfile.TorrentFile{
		Name:        "evil",
		PieceLength: 16,
		Length:      10,
		IsMultiFile: true,
		Files: []torrentfile.FileInfo{
			{Length: 10, Path: []string{"..", "..", "escaped.bin"}},
		},
	}

	if _, err := NewMultiFileWriter(dir, tf); err == nil {
		t.Fatal("NewMultiFileWriter accepted a path escaping the download directory")
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.bin")); err == nil {
		t.Fatal("a file was created outside the download directory")
	}
}

func TestBitfield(t *testing.T) {
	bf := make(Bitfield, 2) // 16 pieces

	for _, i := range []int{0, 7, 8, 15} {
		bf.SetPiece(i)
	}
	for _, i := range []int{0, 7, 8, 15} {
		if !bf.HasPiece(i) {
			t.Errorf("piece %d should be set", i)
		}
	}
	for _, i := range []int{1, 6, 9, 14} {
		if bf.HasPiece(i) {
			t.Errorf("piece %d should not be set", i)
		}
	}

	// Out-of-range indices must be ignored rather than panic: they come from
	// peer-supplied Have messages.
	for _, i := range []int{-1, 16, 1 << 20} {
		if bf.HasPiece(i) {
			t.Errorf("HasPiece(%d) should be false", i)
		}
		bf.SetPiece(i)
	}
}
