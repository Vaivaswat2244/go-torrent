package torrentfile

import (
	"bytes"
	"crypto/sha1"
	"os"
	"path/filepath"
	"testing"

	"github.com/Vaivaswat2244/go-torrent/internal/bencode"
)

func fixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("../../testdata/*.torrent")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	return paths
}

// rawInfoHash computes the info hash the canonical way: by slicing the original
// bytes of the info dictionary straight out of the file. calculateInfoHash
// instead re-encodes the decoded dict, which is only correct if decode/encode
// round-trips byte-for-byte. This asserts the two agree on real torrents.
func rawInfoHash(t *testing.T, data []byte) ([20]byte, bool) {
	t.Helper()
	idx := bytes.Index(data, []byte("4:info"))
	if idx < 0 {
		return [20]byte{}, false
	}
	start := idx + len("4:info")
	_, consumed, err := bencode.DecodeWithLength(data[start:])
	if err != nil {
		return [20]byte{}, false
	}
	return sha1.Sum(data[start : start+consumed]), true
}

func TestOpenFixtures(t *testing.T) {
	for _, path := range fixtures(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			tf, err := Open(path)
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}

			if tf.Name == "" {
				t.Error("empty name")
			}
			if tf.PieceLength <= 0 {
				t.Errorf("piece length = %d", tf.PieceLength)
			}
			if len(tf.PieceHashes) == 0 {
				t.Error("no piece hashes")
			}
			if len(tf.Files) == 0 {
				t.Error("no files")
			}

			// Total length must agree with the file list.
			var sum int
			for _, f := range tf.Files {
				sum += f.Length
			}
			if sum != tf.Length {
				t.Errorf("sum of file lengths %d != Length %d", sum, tf.Length)
			}

			// The piece count must cover exactly the torrent length.
			wantPieces := (tf.Length + tf.PieceLength - 1) / tf.PieceLength
			if len(tf.PieceHashes) != wantPieces {
				t.Errorf("piece count %d, expected %d for length %d / piece %d",
					len(tf.PieceHashes), wantPieces, tf.Length, tf.PieceLength)
			}

			// Every path component must survive validation.
			for _, f := range tf.Files {
				for _, c := range f.Path {
					if err := validatePathComponent(c); err != nil {
						t.Errorf("path component %q from a real torrent was rejected: %v", c, err)
					}
				}
			}
		})
	}
}

func TestInfoHashMatchesRawBytes(t *testing.T) {
	for _, path := range fixtures(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			want, ok := rawInfoHash(t, data)
			if !ok {
				t.Skip("could not locate raw info dict")
			}

			tf, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}

			if tf.InfoHash != want {
				t.Errorf("info hash from re-encoding = %x, but raw info-dict bytes hash to %x",
					tf.InfoHash, want)
			}
		})
	}
}

func TestValidatePathComponent(t *testing.T) {
	bad := []string{
		"", ".", "..", "/etc/passwd", "a/b", `a\b`, "\x00", "sub\x00dir",
	}
	for _, c := range bad {
		if err := validatePathComponent(c); err == nil {
			t.Errorf("validatePathComponent(%q) allowed a unsafe component", c)
		}
	}

	good := []string{"file.txt", "Season 1", "a.b.c", "-weird-name-", "..hidden"}
	for _, c := range good {
		if err := validatePathComponent(c); err != nil {
			t.Errorf("validatePathComponent(%q) rejected a legitimate name: %v", c, err)
		}
	}
}

// A torrent whose file paths escape the output directory must be rejected at
// parse time rather than reaching the file writer.
func TestOpenRejectsPathTraversal(t *testing.T) {
	build := func(path []bencode.Value) []byte {
		info := map[string]bencode.Value{
			"name":         "evil",
			"piece length": int64(16384),
			"pieces":       string(make([]byte, 20)),
			"files": []bencode.Value{
				map[string]bencode.Value{"length": int64(10), "path": path},
			},
		}
		encoded, err := bencode.Encode(map[string]bencode.Value{
			"announce": "http://example.com/announce",
			"info":     info,
		})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}

	cases := map[string][]bencode.Value{
		"parent traversal":   {"..", "..", ".ssh", "authorized_keys"},
		"absolute path":      {"/etc/cron.d/evil"},
		"embedded separator": {"a/../../b"},
		"empty component":    {""},
	}

	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "evil.torrent")
			if err := os.WriteFile(f, build(path), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(f); err == nil {
				t.Fatalf("Open accepted a torrent with path %v", path)
			}
		})
	}
}

// The torrent name becomes the containing directory for multi-file torrents.
func TestOpenRejectsUnsafeName(t *testing.T) {
	encoded, err := bencode.Encode(map[string]bencode.Value{
		"info": map[string]bencode.Value{
			"name":         "..",
			"piece length": int64(16384),
			"pieces":       string(make([]byte, 20)),
			"length":       int64(10),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	f := filepath.Join(t.TempDir(), "n.torrent")
	if err := os.WriteFile(f, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f); err == nil {
		t.Fatal("Open accepted a torrent named \"..\"")
	}
}
