package zvt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Bridge translates between ZVT over a serial line (ECR / cash register side)
// and ZVT over TCP (payment terminal side). APDUs pass through unchanged;
// only framing, CRC and transport-level acknowledgements are added or removed.
type Bridge struct {
	Port       io.ReadWriteCloser
	Dial       func() (net.Conn, error)
	AckTimeout time.Duration // time to wait for the ECR's ACK/NAK per attempt
	InterByte  time.Duration // max. gap between two bytes inside a frame
	Retries    int           // send attempts towards the ECR
	Verbose    bool
	Logf       func(format string, args ...any)

	wmu    sync.Mutex // serial writes
	sendMu sync.Mutex // frames towards the ECR are sent one at a time
	ackCh  chan bool  // ACK (true) / NAK (false) from the ECR

	cmu  sync.Mutex
	conn net.Conn
}

func (b *Bridge) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

func (b *Bridge) logAPDU(dir string, apdu []byte) {
	max := 16
	if b.Verbose {
		max = 0
	}
	b.logf("%s  %-20s %3d B  %s", dir, APDUName(apdu), len(apdu), HexDump(apdu, max))
}

// Run operates the bridge until ctx is cancelled or the serial side fails.
func (b *Bridge) Run(ctx context.Context) error {
	if b.AckTimeout <= 0 {
		b.AckTimeout = 2 * time.Second
	}
	if b.InterByte <= 0 {
		b.InterByte = time.Second
	}
	if b.Retries <= 0 {
		b.Retries = 3
	}
	b.ackCh = make(chan bool, 1)

	bytesCh := make(chan byte, 1024)
	errCh := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := b.Port.Read(buf)
			for i := 0; i < n; i++ {
				select {
				case bytesCh <- buf[i]:
				case <-done:
					return
				}
			}
			if err != nil {
				errCh <- err
				return
			}
		}
	}()

	var d Decoder
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer func() {
		b.cmu.Lock()
		if b.conn != nil {
			b.conn.Close()
			b.conn = nil
		}
		b.cmu.Unlock()
	}()

	for {
		var tch <-chan time.Time
		if d.Busy() {
			tch = timer.C
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			return fmt.Errorf("serial port: %w", err)
		case <-tch:
			b.logf("ECR: incomplete frame (%d B) discarded", len(d.buf))
			d.Reset()
		case c := <-bytesCh:
			ev := d.Feed(c)
			if d.Busy() {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(b.InterByte)
			}
			b.handle(ev)
		}
	}
}

func (b *Bridge) handle(ev Event) {
	switch ev.Kind {
	case EvFrame:
		b.writeSerial([]byte{ACK})
		b.logAPDU("ECR → PT ", ev.APDU)
		if err := b.toTerminal(ev.APDU); err != nil {
			b.logf("PT: send failed: %v", err)
		}
	case EvBadCRC:
		b.writeSerial([]byte{NAK})
		b.logf("ECR: CRC error, NAK sent (%s)", HexDump(ev.APDU, 16))
	case EvAck, EvNak:
		select {
		case b.ackCh <- ev.Kind == EvAck:
		default:
			b.logf("ECR: unexpected %s ignored", map[EventKind]string{EvAck: "ACK", EvNak: "NAK"}[ev.Kind])
		}
	case EvGarbage:
		// A byte outside a frame is answered with NAK, as terminals do. Cash
		// register software relies on that to find the terminal: it writes a
		// single byte to each serial port and takes a lone NAK as the answer
		// of a terminal. The terminal itself is not contacted.
		b.writeSerial([]byte{NAK})
		b.logf("ECR: byte %02X outside a frame, NAK sent", ev.Byte)
	case EvProtocol:
		b.writeSerial([]byte{NAK})
		b.logf("ECR: protocol error, DLE followed by %02X, NAK sent", ev.Byte)
	}
}

func (b *Bridge) writeSerial(p []byte) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	if b.Verbose {
		b.logf("  → ECR (raw)  %s", HexDump(p, 0))
	}
	if _, err := b.Port.Write(p); err != nil {
		b.logf("serial port: write failed: %v", err)
	}
}

// toECR frames an APDU and sends it to the ECR, retrying on NAK or timeout.
func (b *Bridge) toECR(apdu []byte) error {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	frame := EncodeFrame(apdu)
	for attempt := 1; attempt <= b.Retries; attempt++ {
		select { // drop a stale acknowledgement
		case <-b.ackCh:
		default:
		}
		b.writeSerial(frame)
		select {
		case ok := <-b.ackCh:
			if ok {
				return nil
			}
			b.logf("ECR: NAK received, attempt %d/%d", attempt, b.Retries)
		case <-time.After(b.AckTimeout):
			b.logf("ECR: no ACK within %s, attempt %d/%d", b.AckTimeout, attempt, b.Retries)
		}
	}
	return fmt.Errorf("no ACK from ECR after %d attempts", b.Retries)
}

func (b *Bridge) getConn() (net.Conn, error) {
	b.cmu.Lock()
	defer b.cmu.Unlock()
	if b.conn != nil {
		return b.conn, nil
	}
	c, err := b.Dial()
	if err != nil {
		return nil, err
	}
	b.conn = c
	b.logf("PT: connected to %s", c.RemoteAddr())
	go b.readTerminal(c)
	return c, nil
}

func (b *Bridge) dropConn(c net.Conn, reason string) {
	b.cmu.Lock()
	defer b.cmu.Unlock()
	if b.conn == c {
		b.conn = nil
		c.Close()
		b.logf("PT: connection closed (%s)", reason)
	}
}

func (b *Bridge) toTerminal(apdu []byte) error {
	c, err := b.getConn()
	if err != nil {
		return err
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(apdu); err != nil {
		b.dropConn(c, err.Error())
		return err
	}
	return nil
}

func (b *Bridge) readTerminal(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		apdu, err := ReadAPDU(r)
		if err != nil {
			reason := err.Error()
			if errors.Is(err, io.EOF) {
				reason = "closed by terminal"
			}
			b.dropConn(c, reason)
			return
		}
		b.logAPDU("PT  → ECR", apdu)
		if err := b.toECR(apdu); err != nil {
			b.logf("ECR: %v", err)
		}
	}
}
