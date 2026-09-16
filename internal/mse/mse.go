// Package mse implements Message Stream Encryption, the obfuscated BitTorrent
// handshake (also called PE, "protocol encryption") introduced by Azureus and
// supported by every mainstream client.
//
// The point is not confidentiality: the key exchange is unauthenticated, so an
// active attacker can still read the stream. The point is to stop middleboxes
// from fingerprinting BitTorrent. A plain connection opens with the fixed bytes
// "\x13BitTorrent protocol", which is trivial to match and reset. Under MSE
// nothing on the wire is constant: the connection opens with a Diffie-Hellman
// public key and random padding, and everything after that is RC4.
//
// The handshake, with A the initiator and B the receiver:
//
//	1 A->B: Ya, PadA
//	2 B->A: Yb, PadB
//	3 A->B: HASH("req1", S), HASH("req2", SKEY) xor HASH("req3", S),
//	        ENCRYPT(VC, crypto_provide, len(PadC), PadC, len(IA)), ENCRYPT(IA)
//	4 B->A: ENCRYPT(VC, crypto_select, len(PadD), PadD), ENCRYPT2(payload)
//	5 A->B: ENCRYPT2(payload)
//
// S is the shared secret, SKEY is the torrent's info hash, and VC is eight
// zero bytes. The pads are 0-512 bytes of unknown length, so each side finds
// where the next field starts by scanning for a value only the real peer could
// produce: B looks for HASH("req1", S) and A looks for VC as encrypted under
// B's key.
package mse

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"strings"
	"sync"
)

// Method is the crypto method bitfield negotiated in the handshake.
type Method uint32

const (
	// MethodPlaintext obfuscates only the handshake, then drops to plaintext.
	MethodPlaintext Method = 0x01
	// MethodRC4 encrypts the whole connection.
	MethodRC4 Method = 0x02
)

func (m Method) String() string {
	switch m {
	case MethodPlaintext:
		return "plaintext"
	case MethodRC4:
		return "rc4"
	default:
		return fmt.Sprintf("method(%#x)", uint32(m))
	}
}

// Policy decides when encryption is used.
type Policy int

const (
	// PolicyPrefer tries an encrypted connection first and falls back to plain
	// BitTorrent if the peer does not speak MSE. Incoming connections of either
	// kind are accepted.
	PolicyPrefer Policy = iota
	// PolicyRequire refuses unencrypted connections in both directions.
	PolicyRequire
	// PolicyDisable uses plain BitTorrent only.
	PolicyDisable
)

func (p Policy) String() string {
	switch p {
	case PolicyPrefer:
		return "prefer"
	case PolicyRequire:
		return "require"
	case PolicyDisable:
		return "off"
	default:
		return fmt.Sprintf("policy(%d)", int(p))
	}
}

// ParsePolicy reads a policy name as given on the command line.
func ParsePolicy(s string) (Policy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "prefer", "":
		return PolicyPrefer, nil
	case "require", "required", "force":
		return PolicyRequire, nil
	case "off", "disable", "disabled", "none":
		return PolicyDisable, nil
	default:
		return 0, fmt.Errorf("unknown encryption policy %q (want prefer, require or off)", s)
	}
}

const (
	// keySize is the byte length of the 768-bit Diffie-Hellman values.
	keySize = 96
	// maxPad bounds every padding field.
	maxPad = 512
	// discardBytes is how much RC4 keystream is thrown away before use. The
	// first bytes of RC4 output are known to be biased.
	discardBytes = 1024
)

var (
	// prime is the 768-bit modulus the protocol specifies, and generator is 2.
	prime     = mustParseHex("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A63A36210000000000090563")
	generator = big.NewInt(2)

	// primeMinusOne bounds acceptable public keys.
	primeMinusOne = new(big.Int).Sub(prime, big.NewInt(1))

	// vc is the verification constant.
	vc [8]byte
)

func mustParseHex(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		panic("mse: bad prime constant")
	}
	return n
}

func hash(parts ...[]byte) [20]byte {
	h := sha1.New()
	for _, p := range parts {
		h.Write(p)
	}
	var out [20]byte
	copy(out[:], h.Sum(nil))
	return out
}

func xor20(a, b [20]byte) [20]byte {
	var out [20]byte
	for i := range out {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// newKeyPair returns a private exponent and the matching public key, encoded
// as the 96 bytes that go on the wire.
func newKeyPair() (*big.Int, []byte, error) {
	// A 160-bit exponent, as the protocol recommends.
	secret := make([]byte, 20)
	if _, err := rand.Read(secret); err != nil {
		return nil, nil, err
	}
	x := new(big.Int).SetBytes(secret)
	y := new(big.Int).Exp(generator, x, prime)
	return x, leftPad(y.Bytes(), keySize), nil
}

// sharedSecret derives S from the peer's public key. Degenerate keys (0, 1,
// p-1 and anything out of range) would pin S to a value an observer can
// predict, so they are rejected.
func sharedSecret(peerPublic []byte, x *big.Int) ([]byte, error) {
	y := new(big.Int).SetBytes(peerPublic)
	if y.Cmp(big.NewInt(1)) <= 0 || y.Cmp(primeMinusOne) >= 0 {
		return nil, errors.New("mse: peer sent an invalid public key")
	}
	s := new(big.Int).Exp(y, x, prime)
	return leftPad(s.Bytes(), keySize), nil
}

func leftPad(b []byte, size int) []byte {
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}

// newCipher builds the RC4 stream for one direction. The initiator encrypts
// with "keyA" and the receiver with "keyB".
func newCipher(label string, s []byte, infoHash [20]byte) *rc4.Cipher {
	key := hash([]byte(label), s, infoHash[:])
	// rc4.NewCipher only fails for key sizes outside 1..256 bytes.
	c, err := rc4.NewCipher(key[:])
	if err != nil {
		panic(err)
	}
	burn := make([]byte, discardBytes)
	c.XORKeyStream(burn, burn)
	return c
}

// initiatorCiphers returns the initiator's stream pair: it encrypts with keyA
// and decrypts what the receiver sent with keyB. receiverCiphers is the mirror.
//
// Keeping the role-to-label mapping here, rather than at each call site, lets
// the known-answer tests pin it. Getting it backwards on both sides still lets
// this implementation talk to itself, while breaking every other client.
func initiatorCiphers(s []byte, infoHash [20]byte) (enc, dec *rc4.Cipher) {
	return newCipher("keyA", s, infoHash), newCipher("keyB", s, infoHash)
}

func receiverCiphers(s []byte, infoHash [20]byte) (enc, dec *rc4.Cipher) {
	return newCipher("keyB", s, infoHash), newCipher("keyA", s, infoHash)
}

func randomPadLen() (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(maxPad+1))
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// randomPad is sent in the clear, so its content is random rather than zeros.
func randomPad() ([]byte, error) {
	n, err := randomPadLen()
	if err != nil {
		return nil, err
	}
	pad := make([]byte, n)
	if _, err := rand.Read(pad); err != nil {
		return nil, err
	}
	return pad, nil
}

func writeAll(w io.Writer, parts ...[]byte) error {
	var buf bytes.Buffer
	for _, p := range parts {
		buf.Write(p)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// syncOn reads until the last len(pattern) bytes equal pattern, consuming at
// most limit bytes. This is how each side skips the other's padding.
func syncOn(r *bufio.Reader, pattern []byte, limit int) error {
	window := make([]byte, 0, limit)
	for len(window) < limit {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		window = append(window, b)
		if len(window) >= len(pattern) && bytes.Equal(window[len(window)-len(pattern):], pattern) {
			return nil
		}
	}
	return errors.New("mse: could not synchronise with peer")
}

// readDecrypted fills buf from r and decrypts it in place.
func readDecrypted(r io.Reader, c *rc4.Cipher, buf []byte) error {
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	c.XORKeyStream(buf, buf)
	return nil
}

// discardDecrypted skips n bytes of padding. The bytes still have to pass
// through the cipher, or the keystream would fall out of step with the peer.
func discardDecrypted(r io.Reader, c *rc4.Cipher, n int) error {
	if n == 0 {
		return nil
	}
	return readDecrypted(r, c, make([]byte, n))
}

func singleMethod(m Method) bool {
	return m == MethodPlaintext || m == MethodRC4
}

// Initiate runs the initiator side of the handshake for the torrent identified
// by infoHash, offering the methods in provide.
//
// ia is the initial payload, normally our BitTorrent handshake. It travels
// inside step 3, so the peer can answer it without waiting for another round
// trip. The returned connection applies the negotiated method transparently.
func Initiate(conn net.Conn, infoHash [20]byte, provide Method, ia []byte) (*Conn, error) {
	if len(ia) > math.MaxUint16 {
		return nil, errors.New("mse: initial payload too large")
	}
	if provide&(MethodPlaintext|MethodRC4) == 0 {
		return nil, errors.New("mse: no crypto method offered")
	}

	x, ya, err := newKeyPair()
	if err != nil {
		return nil, err
	}
	padA, err := randomPad()
	if err != nil {
		return nil, err
	}

	// Step 1.
	if err := writeAll(conn, ya, padA); err != nil {
		return nil, fmt.Errorf("mse: sending public key: %w", err)
	}

	// Step 2. Only Yb is read here; PadB is skipped later while looking for VC.
	r := bufio.NewReaderSize(conn, 2048)
	yb := make([]byte, keySize)
	if _, err := io.ReadFull(r, yb); err != nil {
		return nil, fmt.Errorf("mse: reading peer public key: %w", err)
	}

	s, err := sharedSecret(yb, x)
	if err != nil {
		return nil, err
	}

	enc, dec := initiatorCiphers(s, infoHash)

	// Step 3.
	req1 := hash([]byte("req1"), s)
	req2 := hash([]byte("req2"), infoHash[:])
	req3 := hash([]byte("req3"), s)
	obfuscated := xor20(req2, req3)

	padCLen, err := randomPadLen()
	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	body.Write(vc[:])
	binary.Write(&body, binary.BigEndian, uint32(provide))
	binary.Write(&body, binary.BigEndian, uint16(padCLen))
	body.Write(make([]byte, padCLen))
	binary.Write(&body, binary.BigEndian, uint16(len(ia)))
	body.Write(ia)

	encrypted := body.Bytes()
	enc.XORKeyStream(encrypted, encrypted)

	if err := writeAll(conn, req1[:], obfuscated[:], encrypted); err != nil {
		return nil, fmt.Errorf("mse: sending handshake: %w", err)
	}

	// Step 4. VC encrypted under B's key is a value only a peer that derived the
	// same secret can send, so finding it marks the end of PadB. Encrypting it
	// here also advances dec past VC, exactly where the next field starts.
	encVC := make([]byte, len(vc))
	dec.XORKeyStream(encVC, vc[:])
	if err := syncOn(r, encVC, maxPad+len(vc)); err != nil {
		return nil, fmt.Errorf("mse: waiting for peer verification: %w", err)
	}

	head := make([]byte, 6)
	if err := readDecrypted(r, dec, head); err != nil {
		return nil, fmt.Errorf("mse: reading method selection: %w", err)
	}
	selected := Method(binary.BigEndian.Uint32(head[0:4]))
	padDLen := int(binary.BigEndian.Uint16(head[4:6]))

	if padDLen > maxPad {
		return nil, fmt.Errorf("mse: peer padding too long (%d)", padDLen)
	}
	if !singleMethod(selected) || selected&provide == 0 {
		return nil, fmt.Errorf("mse: peer selected %v, which we did not offer", selected)
	}
	if err := discardDecrypted(r, dec, padDLen); err != nil {
		return nil, fmt.Errorf("mse: reading peer padding: %w", err)
	}

	return wrap(conn, r, selected, enc, dec, nil), nil
}

// Accept runs the receiver side of the handshake.
//
// infoHashes are the torrents we serve. The initiator names one without
// revealing it on the wire, and Accept reports which. allowed is the set of
// methods we will agree to; RC4 wins when both sides allow it.
//
// Any initial payload the initiator sent is returned as the first bytes read
// from the connection, so the BitTorrent handshake that follows can be read
// normally.
func Accept(conn net.Conn, infoHashes [][20]byte, allowed Method) (*Conn, [20]byte, error) {
	var none [20]byte

	// Step 1.
	r := bufio.NewReaderSize(conn, 2048)
	ya := make([]byte, keySize)
	if _, err := io.ReadFull(r, ya); err != nil {
		return nil, none, fmt.Errorf("mse: reading peer public key: %w", err)
	}

	x, yb, err := newKeyPair()
	if err != nil {
		return nil, none, err
	}
	s, err := sharedSecret(ya, x)
	if err != nil {
		return nil, none, err
	}

	// Step 2.
	padB, err := randomPad()
	if err != nil {
		return nil, none, err
	}
	if err := writeAll(conn, yb, padB); err != nil {
		return nil, none, fmt.Errorf("mse: sending public key: %w", err)
	}

	// Step 3. HASH("req1", S) marks the end of PadA.
	req1 := hash([]byte("req1"), s)
	if err := syncOn(r, req1[:], maxPad+len(req1)); err != nil {
		return nil, none, fmt.Errorf("mse: waiting for peer handshake: %w", err)
	}

	var obfuscated [20]byte
	if _, err := io.ReadFull(r, obfuscated[:]); err != nil {
		return nil, none, fmt.Errorf("mse: reading torrent identifier: %w", err)
	}

	req3 := hash([]byte("req3"), s)
	var infoHash [20]byte
	found := false
	for _, candidate := range infoHashes {
		if xor20(hash([]byte("req2"), candidate[:]), req3) == obfuscated {
			infoHash, found = candidate, true
			break
		}
	}
	if !found {
		return nil, none, errors.New("mse: peer asked for a torrent we do not serve")
	}

	enc, dec := receiverCiphers(s, infoHash)

	head := make([]byte, 14) // VC, crypto_provide, len(PadC)
	if err := readDecrypted(r, dec, head); err != nil {
		return nil, none, fmt.Errorf("mse: reading peer handshake: %w", err)
	}
	if !bytes.Equal(head[0:8], vc[:]) {
		return nil, none, errors.New("mse: bad verification constant")
	}
	provide := Method(binary.BigEndian.Uint32(head[8:12]))
	padCLen := int(binary.BigEndian.Uint16(head[12:14]))

	if padCLen > maxPad {
		return nil, none, fmt.Errorf("mse: peer padding too long (%d)", padCLen)
	}
	if err := discardDecrypted(r, dec, padCLen); err != nil {
		return nil, none, fmt.Errorf("mse: reading peer padding: %w", err)
	}

	lenIA := make([]byte, 2)
	if err := readDecrypted(r, dec, lenIA); err != nil {
		return nil, none, fmt.Errorf("mse: reading payload length: %w", err)
	}
	ia := make([]byte, binary.BigEndian.Uint16(lenIA))
	if err := readDecrypted(r, dec, ia); err != nil {
		return nil, none, fmt.Errorf("mse: reading initial payload: %w", err)
	}

	var selected Method
	switch common := provide & allowed; {
	case common&MethodRC4 != 0:
		selected = MethodRC4
	case common&MethodPlaintext != 0:
		selected = MethodPlaintext
	default:
		return nil, none, fmt.Errorf("mse: no acceptable method (peer offered %#x)", uint32(provide))
	}

	// Step 4. This part is always RC4, whatever method was selected.
	padDLen, err := randomPadLen()
	if err != nil {
		return nil, none, err
	}
	var reply bytes.Buffer
	reply.Write(vc[:])
	binary.Write(&reply, binary.BigEndian, uint32(selected))
	binary.Write(&reply, binary.BigEndian, uint16(padDLen))
	reply.Write(make([]byte, padDLen))

	out := reply.Bytes()
	enc.XORKeyStream(out, out)
	if err := writeAll(conn, out); err != nil {
		return nil, none, fmt.Errorf("mse: sending handshake reply: %w", err)
	}

	return wrap(conn, r, selected, enc, dec, ia), infoHash, nil
}

// Conn is a connection after a completed MSE handshake.
//
// Deadlines, addresses and Close pass straight through to the underlying
// connection. Reads come from the handshake's buffered reader, so bytes the
// peer sent early are not lost.
type Conn struct {
	net.Conn

	method Method
	r      io.Reader

	wmu sync.Mutex
	enc *rc4.Cipher // nil when the stream is plaintext
}

func wrap(conn net.Conn, buffered *bufio.Reader, method Method, enc, dec *rc4.Cipher, ia []byte) *Conn {
	var body io.Reader = buffered
	if method == MethodRC4 {
		body = &cipherReader{r: buffered, c: dec}
	} else {
		enc = nil
	}

	var r io.Reader = body
	if len(ia) > 0 {
		r = io.MultiReader(bytes.NewReader(ia), body)
	}

	return &Conn{Conn: conn, method: method, r: r, enc: enc}
}

// Method reports the negotiated method.
func (c *Conn) Method() Method { return c.method }

func (c *Conn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

// Write encrypts when RC4 was negotiated. The cipher is stateful, so writers
// are serialised; a short write kills the connection, since the two keystreams
// could never be brought back into step.
func (c *Conn) Write(p []byte) (int, error) {
	if c.enc == nil {
		return c.Conn.Write(p)
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()

	buf := make([]byte, len(p))
	c.enc.XORKeyStream(buf, p)
	return c.Conn.Write(buf)
}

type cipherReader struct {
	r io.Reader
	c *rc4.Cipher
}

func (cr *cipherReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.c.XORKeyStream(p[:n], p[:n])
	return n, err
}
