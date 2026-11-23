package zvt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// readEvent reads byte-wise on the ECR side until the decoder yields an event.
func readEvent(t *testing.T, r io.Reader, d *Decoder) Event {
	t.Helper()
	buf := make([]byte, 1)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := r.Read(buf); err != nil {
			t.Fatalf("read on ECR side: %v", err)
		}
		if ev := d.Feed(buf[0]); ev.Kind != EvNone {
			return ev
		}
	}
	t.Fatal("no event on ECR side")
	return Event{}
}

func TestBridgeEndToEnd(t *testing.T) {
	ecrSide, bridgeSide := net.Pipe()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	termConnCh := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			termConnCh <- c
		}
	}()

	b := &Bridge{
		Port:       bridgeSide,
		Dial:       func() (net.Conn, error) { return net.Dial("tcp", ln.Addr().String()) },
		AckTimeout: 200 * time.Millisecond,
		Retries:    3,
		Logf:       t.Logf,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()

	var d Decoder
	// 1. ECR sends Registration, expects ACK; terminal receives the raw APDU.
	reg := []byte{0x06, 0x00, 0x06, 0x12, 0x34, 0x56, 0x78, 0x09, 0x78}
	if _, err := ecrSide.Write(EncodeFrame(reg)); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, ecrSide, &d); ev.Kind != EvAck {
		t.Fatalf("want ACK, got %+v", ev)
	}
	var term net.Conn
	select {
	case term = <-termConnCh:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not connect to the terminal")
	}
	defer term.Close()
	_ = term.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := ReadAPDU(term)
	if err != nil || !bytes.Equal(got, reg) {
		t.Fatalf("terminal received % X, err %v", got, err)
	}

	// 2. Terminal answers 80 00 00; ECR receives a frame and acknowledges it.
	if _, err := term.Write([]byte{0x80, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, ecrSide, &d); ev.Kind != EvFrame || !bytes.Equal(ev.APDU, []byte{0x80, 0x00, 0x00}) {
		t.Fatalf("want frame 80 00 00, got %+v", ev)
	}
	if _, err := ecrSide.Write([]byte{ACK}); err != nil {
		t.Fatal(err)
	}

	// 3. Terminal sends Completion; ECR answers NAK first, then ACK on the retransmission.
	comp := []byte{0x06, 0x0f, 0x00}
	if _, err := term.Write(comp); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, ecrSide, &d); ev.Kind != EvFrame || !bytes.Equal(ev.APDU, comp) {
		t.Fatalf("want Completion, got %+v", ev)
	}
	if _, err := ecrSide.Write([]byte{NAK}); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, ecrSide, &d); ev.Kind != EvFrame || !bytes.Equal(ev.APDU, comp) {
		t.Fatalf("want retransmitted Completion, got %+v", ev)
	}
	if _, err := ecrSide.Write([]byte{ACK}); err != nil {
		t.Fatal(err)
	}

	// 4. ECR acknowledges the Completion on application level with 80 00 00.
	if _, err := ecrSide.Write(EncodeFrame([]byte{0x80, 0x00, 0x00})); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, ecrSide, &d); ev.Kind != EvAck {
		t.Fatalf("want ACK, got %+v", ev)
	}
	_ = term.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got, err := ReadAPDU(term); err != nil || !bytes.Equal(got, []byte{0x80, 0x00, 0x00}) {
		t.Fatalf("terminal received % X, err %v", got, err)
	}

	// 5. ECR sends a frame with a broken CRC and gets NAK.
	bad := EncodeFrame([]byte{0x05, 0x01, 0x00})
	bad[len(bad)-2] ^= 0x01
	if _, err := ecrSide.Write(bad); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, ecrSide, &d); ev.Kind != EvNak {
		t.Fatalf("want NAK, got %+v", ev)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
}

// Cash register software probes a serial port by writing a single byte outside
// a frame and expects NAK, as a terminal answers. The bridge answers on its
// own and leaves the terminal alone.
func TestStrayByteAnsweredWithNAK(t *testing.T) {
	ecrSide, bridgeSide := net.Pipe()
	var dials int32
	b := &Bridge{
		Port: bridgeSide,
		Dial: func() (net.Conn, error) {
			atomic.AddInt32(&dials, 1)
			return nil, errors.New("no terminal in this test")
		},
		AckTimeout: 200 * time.Millisecond,
		Retries:    3,
		Logf:       t.Logf,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	var d Decoder
	if _, err := ecrSide.Write([]byte{0x00}); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, ecrSide, &d); ev.Kind != EvNak {
		t.Fatalf("want NAK, got %+v", ev)
	}
	if n := atomic.LoadInt32(&dials); n != 0 {
		t.Fatalf("terminal dialled %d times for a stray byte", n)
	}
}
