// Package mux implements MyTun's single-port multiplexing protocol.
//
// Wire format (compatible with Bi-Tunnel dev-0910 v1 frames so that old
// clients can still talk to a MyTun server):
//
//	[1 byte type][4 bytes channel id BE][4 bytes payload length BE][payload]
//
// The payload is AES-256-GCM sealed with a key derived from the tunnel
// password (sha256(password)); each frame carries a fresh 12-byte nonce and a
// 16-byte tag, which are concatenated as nonce||ciphertext||tag.
//
// v2 (negotiated, see session.go) keeps the same 9-byte header but moves the
// nonce into a 4-byte frame sequence number and derives per-frame nonces from
// a session salt, which removes one random read and 8 bytes of overhead per
// frame while remaining decryptable by either side.
package mux

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Frame types. Values 1-6 match Bi-Tunnel dev-0910.
const (
	TypeData      byte = 1
	TypeCreate    byte = 2
	TypeClose     byte = 3
	TypeAuth      byte = 4
	TypeAuthRes   byte = 5
	TypeCreateAck byte = 6

	// MyTun extensions (safe: old peers ignore unknown types).
	TypePing byte = 7
	TypePong byte = 8
	TypeHello byte = 9 // protocol negotiation, sent before AUTH
)

const (
	HeaderSize   = 9
	MaxFrameSize = 16 * 1024 * 1024
	nonceSize    = 12
	tagSize      = 16
	maxOverhead  = nonceSize + tagSize
)

var (
	ErrFrameTooLarge = errors.New("mux: frame too large")
	ErrShortFrame    = errors.New("mux: short frame")
	ErrNotSealed     = errors.New("mux: frame not sealed")
)

// DeriveKey matches Bi-Tunnel: sha256(password) as the AES-256 key.
func DeriveKey(password string) []byte {
	sum := sha256.Sum256([]byte(password))
	return sum[:]
}

// Sealer seals and opens frame payloads.
type Sealer struct {
	aead cipher.AEAD
	mu   sync.Mutex
}

// NewSealer builds a sealer. A nil/empty key means "plaintext passthrough",
// which is only used for loopback debugging.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) == 0 {
		return &Sealer{}, nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("mux: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("mux: gcm: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Enabled reports whether payloads are actually encrypted.
func (s *Sealer) Enabled() bool { return s != nil && s.aead != nil }

// Seal appends nonce||ciphertext||tag to dst and returns the extended slice.
func (s *Sealer) Seal(dst, plaintext []byte) ([]byte, error) {
	if !s.Enabled() {
		return append(dst, plaintext...), nil
	}
	start := len(dst)
	dst = append(dst, make([]byte, nonceSize)...)
	nonce := dst[start : start+nonceSize]
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	dst = s.aead.Seal(dst, nonce, plaintext, nil)
	return dst, nil
}

// Open validates and decrypts payload produced by Seal.
func (s *Sealer) Open(payload []byte) ([]byte, error) {
	if !s.Enabled() {
		return payload, nil
	}
	if len(payload) < nonceSize+tagSize {
		return nil, ErrNotSealed
	}
	nonce := payload[:nonceSize]
	return s.aead.Open(nil, nonce, payload[nonceSize:], nil)
}

// Frame is a decoded protocol frame.
type Frame struct {
	Type    byte
	Channel uint32
	Payload []byte
}

// Encoder writes frames to a stream. It is safe for concurrent use so that
// every channel can write without corrupting the shared TCP stream.
type Encoder struct {
	mu     sync.Mutex
	w      *bufio.Writer
	sealer *Sealer
	scratch []byte
	hdr    [HeaderSize]byte
}

// NewEncoder wraps w. bufSize <= 0 uses a 32 KiB buffer.
func NewEncoder(w io.Writer, sealer *Sealer, bufSize int) *Encoder {
	if bufSize <= 0 {
		bufSize = 32 * 1024
	}
	return &Encoder{w: bufio.NewWriterSize(w, bufSize), sealer: sealer}
}

// WriteFrame serializes one frame. Callers keep ownership of payload.
func (e *Encoder) WriteFrame(typ byte, channel uint32, payload []byte) error {
	if len(payload) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	sealed, err := e.sealer.Seal(e.scratch[:0], payload)
	if err != nil {
		return err
	}
	e.scratch = sealed

	binary.BigEndian.PutUint32(e.hdr[1:5], channel)
	binary.BigEndian.PutUint32(e.hdr[5:9], uint32(len(sealed)))
	e.hdr[0] = typ

	if _, err := e.w.Write(e.hdr[:]); err != nil {
		return err
	}
	if len(sealed) > 0 {
		if _, err := e.w.Write(sealed); err != nil {
			return err
		}
	}
	return nil
}

// Buffered reports how many bytes are queued in the write buffer.
func (e *Encoder) Buffered() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.w.Buffered()
}

// Flush pushes buffered bytes to the socket.
func (e *Encoder) Flush() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.w.Flush()
}

// WriteFrameAndFlush is the common case for control frames.
func (e *Encoder) WriteFrameAndFlush(typ byte, channel uint32, payload []byte) error {
	if err := e.WriteFrame(typ, channel, payload); err != nil {
		return err
	}
	return e.Flush()
}

// Decoder reads frames from a stream.
type Decoder struct {
	r      *bufio.Reader
	sealer *Sealer
	hdr    [HeaderSize]byte
	buf    []byte
}

// NewDecoder wraps r.
func NewDecoder(r io.Reader, sealer *Sealer, bufSize int) *Decoder {
	if bufSize <= 0 {
		bufSize = 64 * 1024
	}
	return &Decoder{r: bufio.NewReaderSize(r, bufSize), sealer: sealer}
}

// ReadFrame blocks until a full frame is decoded.
func (d *Decoder) ReadFrame() (Frame, error) {
	if _, err := io.ReadFull(d.r, d.hdr[:]); err != nil {
		return Frame{}, err
	}
	typ := d.hdr[0]
	ch := binary.BigEndian.Uint32(d.hdr[1:5])
	n := int(binary.BigEndian.Uint32(d.hdr[5:9]))

	if n < 0 || n > MaxFrameSize+maxOverhead {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, n)
	}
	if n == 0 {
		return Frame{Type: typ, Channel: ch}, nil
	}

	if cap(d.buf) < n {
		d.buf = make([]byte, n)
	}
	buf := d.buf[:n]
	if _, err := io.ReadFull(d.r, buf); err != nil {
		return Frame{}, err
	}
	plain, err := d.sealer.Open(buf)
	if err != nil {
		return Frame{}, err
	}
	// Copy out: d.buf is reused by the next ReadFrame.
	out := make([]byte, len(plain))
	copy(out, plain)
	return Frame{Type: typ, Channel: ch, Payload: out}, nil
}
