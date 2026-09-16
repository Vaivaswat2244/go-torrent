package mse

import (
	"bytes"
	"crypto/rc4"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// tcpPair returns two ends of a loopback TCP connection. net.Pipe will not do:
// it has no buffering, and both sides of this handshake write padding the
// other has not yet asked for.
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()

	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}

	deadline := time.Now().Add(10 * time.Second)
	client.SetDeadline(deadline)
	server.SetDeadline(deadline)

	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return client, server
}

// recorder captures every byte written through a connection.
type recorder struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.buf.Write(p)
	r.mu.Unlock()
	return r.Conn.Write(p)
}

func (r *recorder) bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf.Bytes()...)
}

var (
	testHash  = [20]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	otherHash = [20]byte{20, 19, 18, 17, 16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
)

// btHandshake builds the 68-byte plain BitTorrent handshake, the thing a
// middlebox fingerprints.
func btHandshake() []byte {
	b := make([]byte, 68)
	b[0] = 19
	copy(b[1:20], "BitTorrent protocol")
	copy(b[28:48], testHash[:])
	copy(b[48:68], "-GT0001-abcdefghijkl")
	return b
}

type acceptResult struct {
	conn     *Conn
	infoHash [20]byte
	err      error
}

func handshake(t *testing.T, provide, allowed Method, served [][20]byte, ia []byte) (*Conn, acceptResult, *recorder, *recorder) {
	t.Helper()

	client, server := tcpPair(t)
	cRec := &recorder{Conn: client}
	sRec := &recorder{Conn: server}

	done := make(chan acceptResult, 1)
	go func() {
		c, h, err := Accept(sRec, served, allowed)
		if err != nil {
			// As a real listener would, hang up so the initiator fails fast.
			server.Close()
		}
		done <- acceptResult{c, h, err}
	}()

	ic, err := Initiate(cRec, testHash, provide, ia)
	res := <-done
	if err != nil && res.err == nil {
		t.Fatalf("Initiate failed but Accept succeeded: %v", err)
	}
	if err != nil {
		return nil, res, cRec, sRec
	}
	return ic, res, cRec, sRec
}

func TestHandshakeRC4RoundTrip(t *testing.T) {
	ia := btHandshake()
	ic, res, _, _ := handshake(t, MethodRC4|MethodPlaintext, MethodRC4|MethodPlaintext,
		[][20]byte{testHash}, ia)
	if res.err != nil {
		t.Fatalf("Accept: %v", res.err)
	}
	if ic == nil {
		t.Fatal("Initiate failed")
	}

	if res.infoHash != testHash {
		t.Errorf("receiver identified %x, want %x", res.infoHash, testHash)
	}
	if ic.Method() != MethodRC4 || res.conn.Method() != MethodRC4 {
		t.Errorf("methods = %v / %v, want rc4 on both sides", ic.Method(), res.conn.Method())
	}

	// The initial payload arrives first on the receiving side.
	got := make([]byte, len(ia))
	if _, err := io.ReadFull(res.conn, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, ia) {
		t.Fatal("initial payload was corrupted")
	}

	exchange(t, ic, res.conn)
}

// exchange checks data flows correctly in both directions, over several writes
// so the keystream has to stay in step across calls.
func exchange(t *testing.T, a, b net.Conn) {
	t.Helper()

	for i := 0; i < 5; i++ {
		msg := bytes.Repeat([]byte{byte(i)}, 1000+i*37)

		if _, err := a.Write(msg); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(b, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, msg) {
			t.Fatalf("a->b message %d corrupted", i)
		}

		reply := append([]byte("reply-"), msg[:10]...)
		if _, err := b.Write(reply); err != nil {
			t.Fatal(err)
		}
		back := make([]byte, len(reply))
		if _, err := io.ReadFull(a, back); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(back, reply) {
			t.Fatalf("b->a message %d corrupted", i)
		}
	}
}

// The whole reason this package exists: the BitTorrent signature must never
// appear on the wire, in either direction.
func TestWireCarriesNoBitTorrentSignature(t *testing.T) {
	ia := btHandshake()
	ic, res, cRec, sRec := handshake(t, MethodRC4, MethodRC4, [][20]byte{testHash}, ia)
	if res.err != nil || ic == nil {
		t.Fatalf("handshake failed: %v", res.err)
	}

	// Carry on as a BitTorrent session would: the receiver answers the
	// handshake, then both sides send ordinary messages.
	if _, err := io.ReadFull(res.conn, make([]byte, len(ia))); err != nil {
		t.Fatal(err)
	}
	if _, err := res.conn.Write(btHandshake()); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(ic, make([]byte, 68)); err != nil {
		t.Fatal(err)
	}
	exchange(t, ic, res.conn)

	signature := []byte("BitTorrent protocol")
	for name, wire := range map[string][]byte{"initiator": cRec.bytes(), "receiver": sRec.bytes()} {
		if bytes.Contains(wire, signature) {
			t.Errorf("%s wire bytes contain the BitTorrent signature", name)
		}
		if bytes.Contains(wire, testHash[:]) {
			t.Errorf("%s wire bytes contain the info hash in the clear", name)
		}
	}
}

// Two handshakes for the same torrent must not start with the same bytes,
// or the opening itself becomes a fingerprint.
func TestOpeningBytesVary(t *testing.T) {
	var openings [][]byte
	for i := 0; i < 2; i++ {
		ic, res, cRec, _ := handshake(t, MethodRC4, MethodRC4, [][20]byte{testHash}, nil)
		if res.err != nil || ic == nil {
			t.Fatalf("handshake %d failed: %v", i, res.err)
		}
		openings = append(openings, cRec.bytes()[:keySize])
	}
	if bytes.Equal(openings[0], openings[1]) {
		t.Error("two handshakes opened with identical bytes")
	}
}

func TestReceiverChoosesPlaintextWhenThatIsAllItAllows(t *testing.T) {
	ic, res, _, _ := handshake(t, MethodRC4|MethodPlaintext, MethodPlaintext, [][20]byte{testHash}, []byte("hello"))
	if res.err != nil || ic == nil {
		t.Fatalf("handshake failed: %v", res.err)
	}
	if ic.Method() != MethodPlaintext || res.conn.Method() != MethodPlaintext {
		t.Fatalf("methods = %v / %v, want plaintext", ic.Method(), res.conn.Method())
	}

	got := make([]byte, 5)
	if _, err := io.ReadFull(res.conn, got); err != nil || string(got) != "hello" {
		t.Fatalf("initial payload = %q, %v", got, err)
	}
	exchange(t, ic, res.conn)
}

// Under the plaintext method, what follows the handshake really is plaintext.
func TestPlaintextMethodSendsPlaintext(t *testing.T) {
	ic, res, cRec, _ := handshake(t, MethodPlaintext, MethodPlaintext, [][20]byte{testHash}, nil)
	if res.err != nil || ic == nil {
		t.Fatalf("handshake failed: %v", res.err)
	}

	marker := []byte("plain-marker-after-handshake")
	if _, err := ic.Write(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(res.conn, make([]byte, len(marker))); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(cRec.bytes(), marker) {
		t.Error("plaintext method should not encrypt the payload")
	}
}

func TestNoCommonMethodFails(t *testing.T) {
	// The initiator only speaks plaintext; the receiver insists on RC4.
	ic, res, _, _ := handshake(t, MethodPlaintext, MethodRC4, [][20]byte{testHash}, nil)
	if res.err == nil {
		t.Error("receiver accepted a peer with no common method")
	}
	if ic != nil {
		t.Error("initiator completed a handshake the receiver refused")
	}
}

func TestUnknownTorrentRejected(t *testing.T) {
	ic, res, _, _ := handshake(t, MethodRC4, MethodRC4, [][20]byte{otherHash}, nil)
	if res.err == nil {
		t.Error("receiver accepted a torrent it does not serve")
	}
	if ic != nil {
		t.Error("initiator completed a handshake for an unknown torrent")
	}
}

// With several torrents served, the receiver must pick the one the initiator
// asked for, even though the info hash never crosses the wire.
func TestReceiverIdentifiesTorrentAmongSeveral(t *testing.T) {
	served := [][20]byte{otherHash, {9, 9, 9}, testHash}
	ic, res, _, _ := handshake(t, MethodRC4, MethodRC4, served, nil)
	if res.err != nil || ic == nil {
		t.Fatalf("handshake failed: %v", res.err)
	}
	if res.infoHash != testHash {
		t.Errorf("identified %x, want %x", res.infoHash, testHash)
	}
}

// Degenerate public keys make the shared secret predictable.
func TestDegeneratePublicKeysRejected(t *testing.T) {
	x, _, err := newKeyPair()
	if err != nil {
		t.Fatal(err)
	}

	bad := map[string][]byte{
		"zero":      leftPad(nil, keySize),
		"one":       leftPad([]byte{1}, keySize),
		"p minus 1": leftPad(primeMinusOne.Bytes(), keySize),
		"p":         leftPad(prime.Bytes(), keySize),
	}
	for name, y := range bad {
		if _, err := sharedSecret(y, x); err == nil {
			t.Errorf("accepted public key %s", name)
		}
	}
}

// Both sides must derive the same secret, and it must be the full 96 bytes
// even when the number happens to have leading zeros.
func TestSharedSecretAgreement(t *testing.T) {
	for i := 0; i < 20; i++ {
		xa, ya, err := newKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		xb, yb, err := newKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		if len(ya) != keySize || len(yb) != keySize {
			t.Fatalf("public key lengths %d/%d", len(ya), len(yb))
		}

		sa, err := sharedSecret(yb, xa)
		if err != nil {
			t.Fatal(err)
		}
		sb, err := sharedSecret(ya, xb)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(sa, sb) || len(sa) != keySize {
			t.Fatal("the two sides derived different secrets")
		}
	}
}

// A peer that is not speaking MSE — here, one sending a plain BitTorrent
// handshake — must produce an error rather than a hang.
func TestInitiateAgainstPlainPeerFails(t *testing.T) {
	client, server := tcpPair(t)

	go func() {
		// A plain peer reads what it thinks is a handshake, dislikes it, and
		// hangs up.
		io.ReadFull(server, make([]byte, 68))
		server.Close()
	}()

	if _, err := Initiate(client, testHash, MethodRC4, btHandshake()); err == nil {
		t.Fatal("handshake with a non-MSE peer should fail")
	}
}

// The receiver must not accept a padding length beyond the protocol limit.
func TestOversizedPaddingRejected(t *testing.T) {
	client, server := tcpPair(t)

	done := make(chan error, 1)
	go func() {
		_, _, err := Accept(server, [][20]byte{testHash}, MethodRC4)
		done <- err
	}()

	// Hand-roll an initiator that claims 513 bytes of PadC.
	x, ya, _ := newKeyPair()
	client.Write(ya)

	yb := make([]byte, keySize)
	if _, err := io.ReadFull(client, yb); err != nil {
		t.Fatal(err)
	}
	s, _ := sharedSecret(yb, x)
	enc := newCipher("keyA", s, testHash)

	req1 := hash([]byte("req1"), s)
	obf := xor20(hash([]byte("req2"), testHash[:]), hash([]byte("req3"), s))

	body := make([]byte, 14)
	binary.BigEndian.PutUint32(body[8:12], uint32(MethodRC4))
	binary.BigEndian.PutUint16(body[12:14], maxPad+1)
	enc.XORKeyStream(body, body)

	client.Write(append(append(req1[:], obf[:]...), body...))

	if err := <-done; err == nil {
		t.Fatal("receiver accepted oversized padding")
	}
}

func TestParsePolicy(t *testing.T) {
	cases := map[string]Policy{
		"prefer": PolicyPrefer, "": PolicyPrefer, "PREFER": PolicyPrefer,
		"require": PolicyRequire, "force": PolicyRequire,
		"off": PolicyDisable, "disable": PolicyDisable,
	}
	for in, want := range cases {
		got, err := ParsePolicy(in)
		if err != nil || got != want {
			t.Errorf("ParsePolicy(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParsePolicy("sometimes"); err == nil {
		t.Error("accepted an unknown policy")
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestKnownAnswers pins the derivations to vectors produced by an independent
// implementation (Python, with RC4 written from scratch).
//
// This matters because the round-trip tests above cannot catch a mistake made
// identically on both sides. Swapping the "keyA" and "keyB" labels, for
// instance, still lets our initiator talk to our receiver, while breaking
// interoperability with every other client. That exact bug was confirmed to
// pass the round-trip tests and fail against a reference implementation.
func TestKnownAnswers(t *testing.T) {
	s := make([]byte, keySize)
	for i := range s {
		s[i] = byte((i*7 + 3) % 256)
	}
	var skey [20]byte
	for i := range skey {
		skey[i] = byte(0xa0 + i)
	}

	const (
		keyAStream = "51c43cc6cd91c6fe480d6e18a34df170"
		keyBStream = "6126e85b8bb448e9b412eb29ac04c2e5"
	)

	stream := func(c *rc4.Cipher) string {
		out := make([]byte, 16)
		c.XORKeyStream(out, out)
		return hex.EncodeToString(out)
	}

	// The initiator sends under keyA and reads under keyB; the receiver is the
	// mirror image. Checking by role, not by label, is what catches a mapping
	// that is backwards on both sides.
	iEnc, iDec := initiatorCiphers(s, skey)
	rEnc, rDec := receiverCiphers(s, skey)

	for name, tc := range map[string]struct {
		c    *rc4.Cipher
		want string
	}{
		"initiator send":    {iEnc, keyAStream},
		"initiator receive": {iDec, keyBStream},
		"receiver send":     {rEnc, keyBStream},
		"receiver receive":  {rDec, keyAStream},
	} {
		if got := stream(tc.c); got != tc.want {
			t.Errorf("%s keystream = %s, want %s", name, got, tc.want)
		}
	}

	req1 := hash([]byte("req1"), s)
	if got, want := hex.EncodeToString(req1[:]), "17fe8d6fb29b3ade48969ce03ba6a7eea841e03e"; got != want {
		t.Errorf("req1 = %s, want %s", got, want)
	}

	obf := xor20(hash([]byte("req2"), skey[:]), hash([]byte("req3"), s))
	if got, want := hex.EncodeToString(obf[:]), "621fb0771a6a000e42a38d265d7a7ed95f1842a2"; got != want {
		t.Errorf("req2 xor req3 = %s, want %s", got, want)
	}
}

// TestDiffieHellmanKnownAnswer checks the group arithmetic, prime included,
// against fixed exponents.
func TestDiffieHellmanKnownAnswer(t *testing.T) {
	if prime.BitLen() != 768 {
		t.Fatalf("prime is %d bits, want 768", prime.BitLen())
	}

	xa, _ := new(big.Int).SetString("1234567890abcdef1234567890abcdef12345678", 16)
	xb, _ := new(big.Int).SetString("fedcba0987654321fedcba0987654321fedcba09", 16)

	ya := leftPad(new(big.Int).Exp(generator, xa, prime).Bytes(), keySize)
	yb := leftPad(new(big.Int).Exp(generator, xb, prime).Bytes(), keySize)

	wantYa := mustHex(t, "c44386497c31c0f76d76479d03a6f40c0cdac6c9a709f493da2c7aa8adb2cbfafe6b7833c0ded4ab5a310db1c2473066f1d3ef0a9e89f1093e7902f72337396dab1d1276ab9e6d447b62e57de71ec5b528209d8fadff8cb9b0d393131aa4a722")
	if !bytes.Equal(ya, wantYa) {
		t.Errorf("Ya = %x", ya)
	}

	wantS := mustHex(t, "7c1798a6ebab579cdd77e332bbab0901538801b6c73ec83c02a470e4d766ae98d8294d822827ecbac1c0bb7c6c7dea25eb6304393ac9c64e7963ef10cdb532c89da9695e550ced358895ad66b16fae3831b600b2d0e0c032203962b5140224b8")
	sa, err := sharedSecret(yb, xa)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := sharedSecret(ya, xb)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sa, wantS) || !bytes.Equal(sb, wantS) {
		t.Errorf("shared secret mismatch:\n a=%x\n b=%x", sa, sb)
	}
}
