package bencode

import (
	"strings"
	"testing"
)

// TestDecodeMalformed covers inputs that must produce an error rather than a
// panic. This decoder consumes untrusted network data (DHT responses, peer
// metadata), so a panic here takes down the whole client.
func TestDecodeMalformed(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		// A negative length made end < start, which the "end > len(data)" guard
		// did not catch, so data[start:end] panicked with a slice-bounds error.
		{"negative string length as dict key", "d-5:xe"},
		{"negative string length nested in list", "l-1:e"},
		{"negative length, plausible payload", "d-10:aaaaaaaaaai1ee"},

		{"dict key is not a string", "di1ei2ee"},
		{"dict key is a list", "dl1:aei1ee"},
		{"truncated string", "10:abc"},
		{"missing colon", "5abc"},
		{"unterminated dict", "d3:foo"},
		{"unterminated list", "l3:foo"},
		{"unterminated integer", "i123"},
		{"empty input", ""},
		{"garbage", "xyz"},
		{"trailing data", "i1ei2e"},
		{"empty length prefix", ":abc"},

		// Unbounded recursion: one stack frame per level.
		{"deeply nested lists", strings.Repeat("l", 5000) + strings.Repeat("e", 5000)},
		{"deeply nested dicts", strings.Repeat("d1:a", 5000) + strings.Repeat("e", 5000)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A panic here fails the test, which is the point.
			if _, err := Decode([]byte(tc.input)); err == nil {
				t.Fatalf("Decode(%q) returned no error, want one", tc.input)
			}
		})
	}
}

func TestDecodeValid(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		v, err := Decode([]byte("4:spam"))
		if err != nil {
			t.Fatal(err)
		}
		if v != "spam" {
			t.Fatalf("got %q, want %q", v, "spam")
		}
	})

	t.Run("empty string", func(t *testing.T) {
		v, err := Decode([]byte("0:"))
		if err != nil {
			t.Fatal(err)
		}
		if v != "" {
			t.Fatalf("got %q, want empty", v)
		}
	})

	t.Run("integers", func(t *testing.T) {
		for input, want := range map[string]int64{"i42e": 42, "i-42e": -42, "i0e": 0} {
			v, err := Decode([]byte(input))
			if err != nil {
				t.Fatalf("%s: %v", input, err)
			}
			if v != want {
				t.Fatalf("%s: got %v, want %d", input, v, want)
			}
		}
	})

	t.Run("binary string data", func(t *testing.T) {
		// Piece hashes are raw bytes and routinely contain 'e', ':' and NUL.
		raw := "3:\x00e:"
		v, err := Decode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if v != "\x00e:" {
			t.Fatalf("got %q, want %q", v, "\x00e:")
		}
	})

	t.Run("nested dict", func(t *testing.T) {
		v, err := Decode([]byte("d4:infod6:lengthi100e4:name3:abcee"))
		if err != nil {
			t.Fatal(err)
		}
		root, ok := v.(map[string]Value)
		if !ok {
			t.Fatalf("got %T, want map", v)
		}
		info, err := GetDict(root, "info")
		if err != nil {
			t.Fatal(err)
		}
		if n, err := GetInt(info, "length"); err != nil || n != 100 {
			t.Fatalf("length: got %d, %v", n, err)
		}
		if s, err := GetString(info, "name"); err != nil || s != "abc" {
			t.Fatalf("name: got %q, %v", s, err)
		}
	})

	t.Run("list", func(t *testing.T) {
		v, err := Decode([]byte("l3:foo3:bare"))
		if err != nil {
			t.Fatal(err)
		}
		list, ok := v.([]Value)
		if !ok || len(list) != 2 || list[0] != "foo" || list[1] != "bar" {
			t.Fatalf("got %#v", v)
		}
	})
}

// DecodeWithLength must report exactly how many bytes the value consumed; BEP 9
// relies on it to find where the bencoded dict ends and raw piece data begins.
func TestDecodeWithLength(t *testing.T) {
	payload := []byte("d8:msg_typei1e5:piecei0ee" + "RAWPIECEDATA")
	_, consumed, err := DecodeWithLength(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(payload[consumed:]); got != "RAWPIECEDATA" {
		t.Fatalf("trailing data = %q, want %q", got, "RAWPIECEDATA")
	}
}

func TestRoundTrip(t *testing.T) {
	original := map[string]Value{
		"name":         "test.iso",
		"piece length": int64(16384),
		"list":         []Value{"a", int64(1)},
		"nested":       map[string]Value{"k": "v"},
	}

	encoded, err := Encode(original)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("failed to decode our own output %q: %v", encoded, err)
	}

	got, ok := decoded.(map[string]Value)
	if !ok {
		t.Fatalf("got %T, want map", decoded)
	}
	if s, _ := GetString(got, "name"); s != "test.iso" {
		t.Fatalf("name = %q", s)
	}
	if n, _ := GetInt(got, "piece length"); n != 16384 {
		t.Fatalf("piece length = %d", n)
	}
}

// Bencode dict keys must be sorted by raw byte order on the wire; the info hash
// depends on it.
func TestEncodeSortsKeys(t *testing.T) {
	encoded, err := Encode(map[string]Value{"b": "2", "a": "1", "C": "3"})
	if err != nil {
		t.Fatal(err)
	}
	want := "d1:C1:31:a1:11:b1:2e"
	if string(encoded) != want {
		t.Fatalf("got %q, want %q", encoded, want)
	}
}

func FuzzDecode(f *testing.F) {
	for _, seed := range []string{
		"d4:infod6:lengthi100eee", "l3:foo3:bare", "i42e", "0:", "d-5:xe",
	} {
		f.Add([]byte(seed))
	}
	// Any input at all must return cleanly rather than panic.
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Decode(data)
	})
}
