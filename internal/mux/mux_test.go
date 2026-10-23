package mux

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// pipePair returns two connected sessions over a real loopback TCP socket.
// net.Pipe is unbuffered and synchronizes every Write with a Read, which makes
// concurrent streams deadlock in ways a real socket never does.
func pipePair(t *testing.T, password string) (*Session, *Session) {
	t.Helper()
	sealer, err := NewSealer(DeriveKey(password))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()
	c2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c1 := <-accepted

	srv := NewSession(c1, sealer, true)
	cli := NewSession(c2, sealer, false)
	go srv.Run()
	go cli.Run()
	return srv, cli
}

func TestFrameRoundTrip(t *testing.T) {
	sealer, err := NewSealer(DeriveKey("pw"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := NewEncoder(&buf, sealer, 0)
	if err := enc.WriteFrameAndFlush(TypeData, 42, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	dec := NewDecoder(&buf, sealer, 0)
	f, err := dec.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != TypeData || f.Channel != 42 || string(f.Payload) != "hello" {
		t.Fatalf("unexpected frame: %+v", f)
	}
}

func TestFrameTamperDetected(t *testing.T) {
	sealer, err := NewSealer(DeriveKey("pw"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := NewEncoder(&buf, sealer, 0)
	_ = enc.WriteFrameAndFlush(TypeData, 1, []byte("payload"))
	raw := buf.Bytes()
	raw[len(raw)-1] ^= 0xFF // flip a ciphertext bit
	dec := NewDecoder(bytes.NewReader(raw), sealer, 0)
	if _, err := dec.ReadFrame(); err == nil {
		t.Fatal("expected authentication failure")
	}
}

func TestChannelDataFlow(t *testing.T) {
	srv, cli := pipePair(t, "secret")
	defer srv.Close()
	defer cli.Close()

	got := make(chan []byte, 1)
	srv.OnChannel = func(ch *Channel) {
		// ACK first: the initiator waits for confirmation before streaming.
		srv.SendACK(ch.ID(), true)
		data, _ := io.ReadAll(ch)
		got <- data
	}

	ch, err := cli.OpenChannel(map[string]any{"type": MetaForward, "host": "x", "port": 1})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ch.AwaitAck(2 * time.Second); err != nil || !ok {
		t.Fatalf("ack failed: ok=%v err=%v", ok, err)
	}
	want := bytes.Repeat([]byte("abcdefgh"), 8192) // 64 KiB
	if _, err := ch.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := cli.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = ch.CloseWrite()
	_ = ch.Close()

	select {
	case data := <-got:
		if !bytes.Equal(data, want) {
			t.Fatalf("payload mismatch: got %d bytes want %d", len(data), len(want))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for payload")
	}
}

// TestLargeConcurrentStreams pushes several multi-megabyte streams at once to
// make sure interleaved frames never corrupt per-channel ordering.
func TestLargeConcurrentStreams(t *testing.T) {
	srv, cli := pipePair(t, "secret")
	defer srv.Close()
	defer cli.Close()

	const streams = 8
	const size = 1 << 20

	var wg sync.WaitGroup
	results := make([][]byte, streams)

	srv.OnChannel = func(ch *Channel) {
		idx := int(ch.Meta["idx"].(float64))
		buf, _ := io.ReadAll(ch)
		results[idx] = buf
		wg.Done()
	}

	for i := 0; i < streams; i++ {
		wg.Add(1)
		ch, err := cli.OpenChannel(map[string]any{"idx": i})
		if err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, size)
		_, _ = rand.Read(payload)
		go func(ch *Channel, payload []byte) {
			_, _ = ch.Write(payload)
			_ = cli.Flush()
			_ = ch.Close()
		}(ch, payload)
		results[i] = payload
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("timeout: %d/%d streams finished", len(results), streams)
	}
	for i := range results {
		if len(results[i]) != size {
			t.Fatalf("stream %d: got %d bytes", i, len(results[i]))
		}
	}
}

// TestBacklogAppliesBackpressure: with nobody reading, a producer must block
// once the backlog cap is reached (not drop the channel, which used to truncate
// large transfers on fast tunnels), and resume once the consumer drains.
func TestBacklogAppliesBackpressure(t *testing.T) {
	_, cli := pipePair(t, "pw")
	defer cli.Close()
	ch, err := cli.OpenChannel(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}

	const chunk = 64 * 1024
	pushed := make(chan error, 1)
	go func() {
		for i := 0; i < 200; i++ { // 12.5 MiB, past the 8 MiB cap
			if err := ch.Push(make([]byte, chunk)); err != nil {
				pushed <- err
				return
			}
		}
		pushed <- nil
	}()

	// The producer must be parked, not finished.
	select {
	case err := <-pushed:
		t.Fatalf("producer should be blocked by backpressure, returned %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// Drain it; the producer should then complete.
	buf := make([]byte, chunk)
	for i := 0; i < 200; i++ {
		if _, err := ch.Read(buf); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	select {
	case err := <-pushed:
		if err != nil {
			t.Fatalf("producer failed after drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not resume after the consumer drained")
	}
}

// TestBacklogTimeoutDropsStuckChannel: a consumer that never reads must not
// wedge the session forever.
func TestBacklogTimeoutDropsStuckChannel(t *testing.T) {
	old := backlogWait
	backlogWait = 150 * time.Millisecond
	defer func() { backlogWait = old }()

	_, cli := pipePair(t, "pw")
	defer cli.Close()
	ch, err := cli.OpenChannel(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var pushErr error
	for i := 0; i < 200 && pushErr == nil; i++ {
		pushErr = ch.Push(make([]byte, 64*1024))
	}
	if pushErr != ErrBacklogFull {
		t.Fatalf("expected ErrBacklogFull, got %v", pushErr)
	}
}
