package mux

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Channel metadata types carried in CREATE frames.
const (
	MetaForward = "forward" // {type,host,port}: open a TCP connection to host:port
	MetaMsg     = "msg"     // {type,id,ts,sender,target}: one-shot text message
	MetaFile    = "file"    // {type,id,name,size,sender,target}: streamed file
)

var (
	ErrSessionClosed = errors.New("mux: session closed")
	ErrChannelClosed = errors.New("mux: channel closed")
	ErrBacklogFull   = errors.New("mux: channel backlog limit reached")
	ErrNotAuthed     = errors.New("mux: not authenticated")
)

// maxChannelBacklog bounds per-channel buffering so a slow reader cannot
// exhaust memory. This is a safety valve, not normal flow control: real
// backpressure comes from the peer's TCP window once io.Copy blocks.
const maxChannelBacklog = 2 << 20 // 2 MiB

// Channel is a bidirectional byte stream multiplexed over a Session. It
// implements net.Conn so it can be handed to io.Copy directly.
type Channel struct {
	session *Session
	id      uint32
	Meta    map[string]any

	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	readOff int
	closed  bool
	remote  bool
	err     error

	localAddr  net.Addr
	remoteAddr net.Addr
	createdAt  time.Time
	role       string
	bytesIn    atomic.Int64
	bytesOut   atomic.Int64
	ackOnce    sync.Once
	ackCh      chan bool
}

func newChannel(s *Session, id uint32, meta map[string]any) *Channel {
	role := ""
	if meta != nil {
		if v, ok := meta["type"].(string); ok {
			role = v
		}
	}
	c := &Channel{
		session:   s,
		id:        id,
		Meta:      meta,
		role:      role,
		ackCh:     make(chan bool, 1),
		createdAt: time.Now(),
	}
	c.cond = sync.NewCond(&c.mu)
	if s.conn != nil {
		c.localAddr = s.conn.LocalAddr()
		c.remoteAddr = s.conn.RemoteAddr()
	}
	return c
}

// ID returns the wire channel id.
func (c *Channel) ID() uint32 { return c.id }

// CreatedAt exposes the channel creation time for stats.
func (c *Channel) CreatedAt() time.Time { return c.createdAt }

// BytesIn/BytesOut report transferred volume.
func (c *Channel) BytesIn() int64  { return c.bytesIn.Load() }
func (c *Channel) BytesOut() int64 { return c.bytesOut.Load() }

// Push enqueues payload received from the peer.
func (c *Channel) Push(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrChannelClosed
	}
	backlog := len(c.buf) - c.readOff + len(p)
	if backlog > maxChannelBacklog {
		c.closed = true
		c.err = ErrBacklogFull
		c.cond.Broadcast()
		return ErrBacklogFull
	}
	c.buf = append(c.buf, p...)
	c.cond.Broadcast()
	return nil
}

// Read implements io.Reader.
func (c *Channel) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if c.readOff < len(c.buf) {
			n := copy(b, c.buf[c.readOff:])
			c.readOff += n
			if c.readOff == len(c.buf) {
				c.buf = c.buf[:0]
				c.readOff = 0
			} else if c.readOff > 1<<20 {
				c.buf = append(c.buf[:0], c.buf[c.readOff:]...)
				c.readOff = 0
			}
			c.bytesIn.Add(int64(n))
			return n, nil
		}
		if c.closed {
			if c.err != nil {
				return 0, c.err
			}
			return 0, io.EOF
		}
		c.cond.Wait()
	}
}

// Write implements io.Writer by emitting DATA frames.
func (c *Channel) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	// Chunk large writes so peers with 16 MiB frame limits stay happy.
	const chunk = 64 * 1024
	written := 0
	for written < len(b) {
		n := len(b) - written
		if n > chunk {
			n = chunk
		}
		// Fair-queue admission: blocking here is what keeps one bulk transfer
		// from starving an interactive channel sharing the same tunnel.
		if c.session.limiter != nil {
			c.session.limiter.Admit(c.id, c.role, n)
		}
		if err := c.session.writeFrame(TypeData, c.id, b[written:written+n]); err != nil {
			return written, err
		}
		written += n
	}
	c.bytesOut.Add(int64(written))
	// Low-latency path: ship small interactive writes immediately instead of
	// waiting for the 64 KiB buffer to fill. Bulk transfers keep buffering so
	// they stay at line rate.
	if c.session.enc.Buffered() < 16*1024 {
		if err := c.session.enc.Flush(); err != nil {
			return written, err
		}
	}
	return written, nil
}

// Close sends CLOSE to the peer and releases the channel.
func (c *Channel) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	alreadyRemote := c.remote
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()

	c.session.unregisterLimiter(c.id)
	if !alreadyRemote {
		_ = c.session.writeFrame(TypeClose, c.id, nil)
		// Control frames must hit the wire immediately: without this the peer
		// never sees the half of the stream that was still sitting in the write
		// buffer, and io.ReadAll on the far side blocks forever.
		_ = c.session.enc.Flush()
	}
	c.session.dropChannel(c.id)
	return nil
}

// CloseWrite is a no-op helper for half-close semantics.
func (c *Channel) CloseWrite() error { return nil }

// markRemoteClose records that the peer closed the channel.
func (c *Channel) markRemoteClose() {
	c.mu.Lock()
	c.remote = true
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()
}

// notifyAck delivers the CREATE_ACK result exactly once.
func (c *Channel) notifyAck(ok bool) {
	c.ackOnce.Do(func() { c.ackCh <- ok })
}

// AwaitAck waits for the remote side to confirm the channel is usable.
func (c *Channel) AwaitAck(timeout time.Duration) (bool, error) {
	select {
	case ok := <-c.ackCh:
		return ok, nil
	case <-time.After(timeout):
		return false, errors.New("tunnel channel ack timeout")
	case <-c.session.closedCh:
		return false, ErrSessionClosed
	}
}

func (c *Channel) LocalAddr() net.Addr                { return c.localAddr }
func (c *Channel) RemoteAddr() net.Addr               { return c.remoteAddr }
func (c *Channel) SetDeadline(t time.Time) error      { return nil }
func (c *Channel) SetReadDeadline(t time.Time) error  { return nil }
func (c *Channel) SetWriteDeadline(t time.Time) error { return nil }

// Limiter shapes outbound DATA frames. Implemented by internal/qos; kept as an
// interface so the wire protocol package stays dependency-free.
type Limiter interface {
	// Admit blocks until n bytes may be sent on the channel.
	Admit(id uint32, role string, n int) bool
	// Register announces a channel and its role to the classifier.
	Register(id uint32, role string)
	// Unregister forgets a channel.
	Unregister(id uint32)
}

// Session is one authenticated multiplexed tunnel connection.
type Session struct {
	conn     net.Conn
	enc      *Encoder
	dec      *Decoder
	sealer   *Sealer
	isServer bool
	role     string

	nextID   atomic.Uint32
	channels sync.Map // uint32 -> *Channel

	authed   atomic.Bool
	closedCh chan struct{}
	closeOnce sync.Once
	closeErr  error

	// handlers
	OnChannel  func(*Channel)
	OnClose    func(*Session)
	OnAuth     func(password, clientID, proto string) bool
	OnAuthDone func(ok bool)

	// identity (set by tunnel layer)
	ClientID   string
	RemoteAddr string
	ConnectedAt time.Time
	Proto      string

	lastActivity atomic.Int64
	msgCount     atomic.Int64

	// limiter optionally shapes outbound DATA frames (nil = unshaped).
	limiter Limiter
}

// NewSession wires a Session over conn. buffered writes are used, so callers
// must run Run() and periodically Flush (data frames flush automatically when
// the buffer fills).
func NewSession(conn net.Conn, sealer *Sealer, isServer bool) *Session {
	s := &Session{
		conn:        conn,
		sealer:      sealer,
		isServer:    isServer,
		closedCh:    make(chan struct{}),
		ConnectedAt: time.Now(),
	}
	if isServer {
		s.role = "server"
		s.nextID.Store(2)
	} else {
		s.role = "client"
		s.nextID.Store(1)
	}
	s.enc = NewEncoder(conn, sealer, 64*1024)
	s.dec = NewDecoder(conn, sealer, 64*1024)
	return s
}

// SetLimiter attaches an outbound shaper. Safe to call before Run only.
func (s *Session) SetLimiter(l Limiter) { s.limiter = l }

// Role reports "server" or "client".
func (s *Session) Role() string { return s.role }

// Conn exposes the underlying connection (TLS).
func (s *Session) Conn() net.Conn { return s.conn }

// IsAuthed reports whether the session completed authentication.
func (s *Session) IsAuthed() bool { return s.authed.Load() }

// SetAuthed marks the session authenticated.
func (s *Session) SetAuthed(v bool) { s.authed.Store(v) }

// Close tears down the session and all channels.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.closedCh)
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.channels.Range(func(k, v any) bool {
			if ch, ok := v.(*Channel); ok {
				ch.markRemoteClose()
			}
			s.channels.Delete(k)
			return true
		})
		if s.OnClose != nil {
			s.OnClose(s)
		}
	})
	return nil
}

// Done returns a channel closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.closedCh }

func (s *Session) writeFrame(typ byte, id uint32, payload []byte) error {
	select {
	case <-s.closedCh:
		return ErrSessionClosed
	default:
	}
	err := s.enc.WriteFrame(typ, id, payload)
	if err == nil && typ == TypeData {
		// Flushing every data frame would defeat buffering; the bufio writer
		// flushes when full. Control frames flush explicitly below.
		s.lastActivity.Store(time.Now().UnixNano())
	}
	return err
}

// Run reads frames until the connection dies. It must be called once.
func (s *Session) Run() error {
	defer s.Close()
	for {
		f, err := s.dec.ReadFrame()
		if err != nil {
			s.closeErr = err
			return err
		}
		s.lastActivity.Store(time.Now().UnixNano())
		switch f.Type {
		case TypeAuth:
			s.handleAuth(f.Payload)
		case TypeAuthRes:
			ok := len(f.Payload) > 0 && f.Payload[0] == 1
			s.authed.Store(ok)
			if s.OnAuthDone != nil {
				s.OnAuthDone(ok)
			}
		case TypeCreate:
			s.handleCreate(f)
		case TypeData:
			if v, ok := s.channels.Load(f.Channel); ok {
				ch := v.(*Channel)
				if err := ch.Push(f.Payload); err != nil && errors.Is(err, ErrBacklogFull) {
					_ = ch.Close()
				}
			}
		case TypeClose:
			if v, ok := s.channels.Load(f.Channel); ok {
				ch := v.(*Channel)
				ch.markRemoteClose()
				_ = ch
				s.dropChannel(f.Channel)
			}
		case TypeCreateAck:
			if v, ok := s.channels.Load(f.Channel); ok {
				ch := v.(*Channel)
				ok := len(f.Payload) > 0 && f.Payload[0] == 1
				ch.notifyAck(ok)
				if !ok {
					ch.markRemoteClose()
				}
			}
		case TypePing:
			_ = s.enc.WriteFrameAndFlush(TypePong, f.Channel, nil)
		case TypePong:
			// activity timestamp already updated
		case TypeHello:
			s.handleHello(f.Payload)
		default:
			// ignore unknown extension frames
		}
	}
}

func (s *Session) handleHello(payload []byte) {
	var hello struct {
		Proto string `json:"proto"`
	}
	if err := json.Unmarshal(payload, &hello); err == nil && hello.Proto != "" {
		s.Proto = hello.Proto
	}
	if s.isServer && s.Proto == "" {
		// Announce our capability to peers that only speak v1.
		_ = s.enc.WriteFrameAndFlush(TypeHello, 0, []byte(`{"proto":"mytun-2"}`))
	}
}

func (s *Session) handleAuth(payload []byte) {
	var auth struct {
		Password string `json:"password"`
		ClientID string `json:"clientId"`
		Proto    string `json:"proto"`
	}
	if err := json.Unmarshal(payload, &auth); err != nil || auth.Password == "" {
		auth.Password = string(payload)
		auth.ClientID = "legacy-client"
	}
	if auth.ClientID == "" {
		auth.ClientID = "legacy-client"
	}
	if auth.Proto != "" {
		s.Proto = auth.Proto
	}
	ok := true
	if s.OnAuth != nil {
		ok = s.OnAuth(auth.Password, auth.ClientID, s.Proto)
	}
	if ok {
		s.ClientID = auth.ClientID
		s.authed.Store(true)
	}
	// AUTH_RES payload: 1 byte ok. v2 peers additionally get the negotiated
	// protocol string appended as JSON, which v1 peers simply ignore.
	res := []byte{0}
	if ok {
		res = []byte{1}
		if s.Proto != "" {
			res = append(res, []byte(`{"proto":"mytun-2"}`)...)
		}
	}
	_ = s.enc.WriteFrameAndFlush(TypeAuthRes, 0, res)
}

func (s *Session) handleCreate(f Frame) {
	var meta map[string]any
	if len(f.Payload) > 0 {
		_ = json.Unmarshal(f.Payload, &meta)
	}
	if meta == nil {
		meta = map[string]any{}
	}
	ch := newChannel(s, f.Channel, meta)
	s.channels.Store(f.Channel, ch)
	if s.limiter != nil {
		s.limiter.Register(f.Channel, ch.role)
	}
	if s.OnChannel != nil {
		go s.OnChannel(ch)
	}
}

// OpenChannel allocates a channel and notifies the peer.
func (s *Session) OpenChannel(meta map[string]any) (*Channel, error) {
	select {
	case <-s.closedCh:
		return nil, ErrSessionClosed
	default:
	}
	id := s.nextID.Add(2) - 2
	if id == 0 || id > 0xFFFFFFF0 {
		s.nextID.Store(1)
		id = s.nextID.Add(2) - 2
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	ch := newChannel(s, id, meta)
	s.channels.Store(id, ch)
	if s.limiter != nil {
		s.limiter.Register(id, ch.role)
	}
	if err := s.enc.WriteFrameAndFlush(TypeCreate, id, payload); err != nil {
		s.channels.Delete(id)
		return nil, err
	}
	return ch, nil
}

func (s *Session) dropChannel(id uint32) {
	s.channels.Delete(id)
	s.unregisterLimiter(id)
}

func (s *Session) unregisterLimiter(id uint32) {
	if s.limiter != nil {
		s.limiter.Unregister(id)
	}
}

// SendACK answers a CREATE with success/failure.
func (s *Session) SendACK(id uint32, ok bool) {
	b := []byte{0}
	if ok {
		b = []byte{1}
	}
	_ = s.enc.WriteFrameAndFlush(TypeCreateAck, id, b)
}

// SendAuth performs client-side authentication.
func (s *Session) SendAuth(password, clientID, proto string) error {
	body := map[string]string{"password": password, "clientId": clientID}
	if proto != "" {
		body["proto"] = proto
	}
	b, _ := json.Marshal(body)
	return s.enc.WriteFrameAndFlush(TypeAuth, 0, b)
}

// SendMsg opens a one-shot channel carrying text.
func (s *Session) SendMsg(meta map[string]any, text []byte) error {
	ch, err := s.OpenChannel(meta)
	if err != nil {
		return err
	}
	defer ch.Close()
	if _, err := ch.Write(text); err != nil {
		return err
	}
	return s.enc.Flush()
}

// Flush forces buffered frames out.
func (s *Session) Flush() error { return s.enc.Flush() }

// ChannelCount reports live channels.
func (s *Session) ChannelCount() int {
	n := 0
	s.channels.Range(func(_, _ any) bool { n++; return true })
	return n
}

// Channels returns a snapshot of live channels.
func (s *Session) Channels() []*Channel {
	var out []*Channel
	s.channels.Range(func(_, v any) bool {
		if ch, ok := v.(*Channel); ok {
			out = append(out, ch)
		}
		return true
	})
	return out
}

// LastActivity returns the last frame timestamp.
func (s *Session) LastActivity() time.Time {
	v := s.lastActivity.Load()
	if v == 0 {
		return s.ConnectedAt
	}
	return time.Unix(0, v)
}

// KeepAlive sends periodic pings until the session ends.
func (s *Session) KeepAlive(every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.closedCh:
			return
		case <-t.C:
			if err := s.enc.WriteFrameAndFlush(TypePing, 0, nil); err != nil {
				return
			}
		}
	}
}
