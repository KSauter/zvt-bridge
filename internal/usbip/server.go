package usbip

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// Device is a USB device emulated in software. One Device is served per Server.
//
// Control handles endpoint-0 requests synchronously. Submit hands over a bulk
// or interrupt transfer; the device completes it with Request.Complete, now or
// later, unless Unlink cancels it first. Attached and Detached bracket a
// client session (the device being plugged in and unplugged).
type Device interface {
	Info() DeviceInfo
	Control(setup [8]byte, data []byte) ([]byte, error)
	Submit(req *Request)
	Unlink(seq uint32) bool
	Attached()
	Detached()
}

// Request is a pending bulk or interrupt transfer.
type Request struct {
	Seq    uint32
	EP     uint8 // endpoint number without direction bit
	In     bool  // device → host
	Length int   // bytes requested (IN) or supplied (OUT)
	Data   []byte

	conn *conn
	done atomic.Bool
}

// Complete answers the transfer. For IN transfers data is sent to the host
// (truncated to Length); for OUT transfers Length bytes are reported as moved.
// Only the first call has an effect.
func (r *Request) Complete(status int32, data []byte) {
	if !r.done.CompareAndSwap(false, true) {
		return
	}
	if r.In {
		if len(data) > r.Length {
			data = data[:r.Length]
		}
		r.conn.send(encodeRetSubmit(r.Seq, status, data))
		return
	}
	r.conn.send(encodeRetSubmitOut(r.Seq, status, r.Length))
}

// Server listens for a USB/IP client and serves one Device.
type Server struct {
	Addr   string // e.g. 127.0.0.1:3240
	Device Device
	Logf   func(format string, args ...any)
	Trace  bool // log unlinks and unsupported transfers

	mu       sync.Mutex
	imported *conn
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// ListenAndServe accepts connections until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve accepts connections on ln until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
		s.mu.Lock()
		if s.imported != nil {
			s.imported.nc.Close()
		}
		s.mu.Unlock()
	}()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if tc, ok := nc.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		go s.handle(&conn{nc: nc, srv: s})
	}
}

type conn struct {
	nc  net.Conn
	srv *Server
	wmu sync.Mutex
}

func (c *conn) send(b []byte) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.nc.Write(b); err != nil {
		c.srv.logf("usbip: write to client failed: %v", err)
		c.nc.Close()
	}
}

func (s *Server) handle(c *conn) {
	defer c.nc.Close()
	var oph [opHeaderSize]byte
	if _, err := io.ReadFull(c.nc, oph[:]); err != nil {
		return
	}
	code := binary.BigEndian.Uint16(oph[2:])
	info := s.Device.Info()
	switch code {
	case OpReqDevlist:
		out := encodeOpHeader(OpRepDevlist, 0)
		out = binary.BigEndian.AppendUint32(out, 1)
		out = append(out, info.encode(true)...)
		c.send(out)
		return
	case OpReqImport:
		var busid [32]byte
		if _, err := io.ReadFull(c.nc, busid[:]); err != nil {
			return
		}
		want := string(trimNul(busid[:]))
		s.mu.Lock()
		busy := s.imported != nil
		if want == info.BusID && !busy {
			s.imported = c
		}
		s.mu.Unlock()
		if want != info.BusID || busy {
			s.logf("usbip: import of %q refused (have %q, in use: %v)", want, info.BusID, busy)
			c.send(encodeOpHeader(OpRepImport, 1))
			return
		}
		c.send(append(encodeOpHeader(OpRepImport, 0), info.encode(false)...))
		s.logf("usbip: device %s imported by %s", info.BusID, c.nc.RemoteAddr())
		s.Device.Attached()
		err := s.urbLoop(c)
		s.Device.Detached()
		s.mu.Lock()
		if s.imported == c {
			s.imported = nil
		}
		s.mu.Unlock()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			s.logf("usbip: session with %s ended: %v", c.nc.RemoteAddr(), err)
		} else {
			s.logf("usbip: device %s released by %s", info.BusID, c.nc.RemoteAddr())
		}
	default:
		s.logf("usbip: unknown operation %#x from %s", code, c.nc.RemoteAddr())
	}
}

func trimNul(b []byte) []byte {
	for i, x := range b {
		if x == 0 {
			return b[:i]
		}
	}
	return b
}

func (s *Server) urbLoop(c *conn) error {
	for {
		h, err := readHeader(c.nc)
		if err != nil {
			return err
		}
		switch h.Command {
		case CmdSubmit:
			if err := s.submit(c, h); err != nil {
				return err
			}
		case CmdUnlink:
			status := StatusOK
			if s.Device.Unlink(h.UnlinkSeq) {
				status = StatusECONNRESET
			}
			if s.Trace {
				s.logf("usbip: unlink of seq %d (pending: %v)", h.UnlinkSeq, status == StatusECONNRESET)
			}
			c.send(encodeRetUnlink(h.Seq, status))
		}
	}
}

func (s *Server) submit(c *conn, h *header) error {
	if h.TransferLength < 0 || h.TransferLength > maxTransfer {
		return fmt.Errorf("usbip: transfer length %d out of range", h.TransferLength)
	}
	var data []byte
	if h.Direction == DirOut && h.TransferLength > 0 {
		data = make([]byte, h.TransferLength)
		if _, err := io.ReadFull(c.nc, data); err != nil {
			return err
		}
	}
	// Isochronous descriptors follow the payload; we don't support ISO but must stay in sync.
	if h.NumPackets > 0 && h.NumPackets != -1 {
		if _, err := io.CopyN(io.Discard, c.nc, int64(h.NumPackets)*16); err != nil {
			return err
		}
		if s.Trace {
			s.logf("usbip: isochronous transfer on endpoint %d refused", h.EP)
		}
		c.send(encodeRetSubmit(h.Seq, StatusEPIPE, nil))
		return nil
	}
	if h.EP == 0 {
		reply, err := s.Device.Control(h.Setup, data)
		if err != nil {
			c.send(encodeRetSubmit(h.Seq, StatusEPIPE, nil))
			return nil
		}
		if h.Direction == DirIn {
			if len(reply) > int(h.TransferLength) {
				reply = reply[:h.TransferLength]
			}
			c.send(encodeRetSubmit(h.Seq, StatusOK, reply))
		} else {
			c.send(encodeRetSubmitOut(h.Seq, StatusOK, len(data)))
		}
		return nil
	}
	req := &Request{Seq: h.Seq, EP: uint8(h.EP), In: h.Direction == DirIn, Length: int(h.TransferLength), Data: data, conn: c}
	s.Device.Submit(req)
	return nil
}
