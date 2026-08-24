package p2p

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
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

// ReadBlock serves peer requests, so it must return sub-ranges correctly even
// when a block straddles a file boundary.
func TestReadBlock(t *testing.T) {
	dir := t.TempDir()
	tf := testTorrent() // piece length 16, files of 10/20/5
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

	cases := []struct{ piece, begin, length int }{
		{0, 0, 4},  // start of piece 0, inside file 0
		{0, 8, 8},  // straddles file 0 -> file 1
		{0, 10, 6}, // entirely in file 1, offset into the piece
		{1, 0, 16}, // whole of piece 1, straddles file 1 -> file 2
		{1, 12, 4}, // tail of piece 1, inside file 2
		{2, 0, 3},  // the short final piece
		{2, 1, 2},  // offset within the short final piece
	}

	for _, c := range cases {
		got, err := mw.ReadBlock(c.piece, c.begin, c.length)
		if err != nil {
			t.Errorf("ReadBlock(%d,%d,%d): %v", c.piece, c.begin, c.length, err)
			continue
		}
		offset := c.piece*tf.PieceLength + c.begin
		want := data[offset : offset+c.length]
		if !bytes.Equal(got, want) {
			t.Errorf("ReadBlock(%d,%d,%d) = %v, want %v", c.piece, c.begin, c.length, got, want)
		}
	}

	// Reading past the end of the torrent must error, not return short data.
	if _, err := mw.ReadBlock(2, 0, 16); err == nil {
		t.Error("ReadBlock past the end of the torrent should fail")
	}
	for _, bad := range [][3]int{{-1, 0, 4}, {0, -1, 4}, {0, 0, 0}} {
		if _, err := mw.ReadBlock(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("ReadBlock%v should be rejected", bad)
		}
	}
}

// TestMultiFileWriterConcurrent is the analogue of TestSafeBitfieldConcurrent:
// it reproduces the access pattern seeding introduces, where uploader goroutines
// read blocks while the download loop writes pieces.
//
// Run with -race. On the old Seek-then-Read/Write implementation the shared
// per-file cursor made readers and writers interleave, silently returning the
// wrong bytes.
func TestMultiFileWriterConcurrent(t *testing.T) {
	dir := t.TempDir()

	// Wide enough that many pieces are in flight at once.
	tf := &torrentfile.TorrentFile{
		Name:        "concurrent",
		PieceLength: 64,
		Length:      64 * 32,
		IsMultiFile: true,
		Files: []torrentfile.FileInfo{
			{Length: 700, Path: []string{"a.bin"}},
			{Length: 900, Path: []string{"sub", "b.bin"}},
			{Length: 448, Path: []string{"c.bin"}},
		},
	}
	data := payload(tf.Length)

	mw, err := NewMultiFileWriter(dir, tf)
	if err != nil {
		t.Fatal(err)
	}
	defer mw.Close()

	numPieces := tf.Length / tf.PieceLength

	// Pre-write the first half so readers have verified content to check.
	for i := 0; i < numPieces/2; i++ {
		if err := mw.WritePiece(i, data[i*tf.PieceLength:(i+1)*tf.PieceLength]); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup

	// Writers: the download loop storing the second half.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := numPieces/2 + offset; i < numPieces; i += 4 {
				if err := mw.WritePiece(i, data[i*tf.PieceLength:(i+1)*tf.PieceLength]); err != nil {
					t.Errorf("WritePiece(%d): %v", i, err)
					return
				}
			}
		}(w)
	}

	// Readers: uploaders serving 16-byte blocks out of the settled first half.
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for n := 0; n < 150; n++ {
				piece := (seed*7 + n) % (numPieces / 2)
				begin := (n % 4) * 16
				got, err := mw.ReadBlock(piece, begin, 16)
				if err != nil {
					t.Errorf("ReadBlock(%d,%d,16): %v", piece, begin, err)
					return
				}
				offset := piece*tf.PieceLength + begin
				if !bytes.Equal(got, data[offset:offset+16]) {
					t.Errorf("ReadBlock(%d,%d,16) returned the wrong bytes", piece, begin)
					return
				}
			}
		}(r)
	}

	wg.Wait()

	// Everything must read back intact afterwards.
	for i := 0; i < numPieces; i++ {
		got, err := mw.ReadPiece(i, tf.PieceLength)
		if err != nil {
			t.Fatalf("ReadPiece(%d): %v", i, err)
		}
		if !bytes.Equal(got, data[i*tf.PieceLength:(i+1)*tf.PieceLength]) {
			t.Fatalf("piece %d is corrupt after concurrent access", i)
		}
	}
}
