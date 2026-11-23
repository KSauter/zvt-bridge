package usbip

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// client is a minimal USB/IP client playing the role of vhci-hcd / usbip-win2.
type client struct {
	t    *testing.T
	nc   net.Conn
	seq  uint32
	dirs map[uint32]uint32 // seqnum → direction, to know whether RET_SUBMIT carries data
}

func newClient(t *testing.T, addr string) *client {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.SetDeadline(time.Now().Add(5 * time.Second))
	return &client{t: t, nc: nc, dirs: map[uint32]uint32{}}
}

func (c *client) write(b []byte) {
	c.t.Helper()
	if _, err := c.nc.Write(b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) read(n int) []byte {
	c.t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(c.nc, b); err != nil {
		c.t.Fatalf("read %d bytes: %v", n, err)
	}
	return b
}

func (c *client) importDevice(busid string) []byte {
	c.t.Helper()
	req := encodeOpHeader(OpReqImport, 0)
	id := make([]byte, 32)
	copy(id, busid)
	c.write(append(req, id...))
	rep := c.read(opHeaderSize)
	if code := binary.BigEndian.Uint16(rep[2:]); code != OpRepImport {
		c.t.Fatalf("reply code %#x", code)
	}
	if status := binary.BigEndian.Uint32(rep[4:]); status != 0 {
		return nil
	}
	return c.read(312)
}

func (c *client) submit(dir uint32, ep uint32, length int, setup [8]byte, data []byte) uint32 {
	c.t.Helper()
	c.seq++
	h := make([]byte, headerSize)
	binary.BigEndian.PutUint32(h[0:], CmdSubmit)
	binary.BigEndian.PutUint32(h[4:], c.seq)
	binary.BigEndian.PutUint32(h[8:], 1<<16|2)
	binary.BigEndian.PutUint32(h[12:], dir)
	binary.BigEndian.PutUint32(h[16:], ep)
	binary.BigEndian.PutUint32(h[24:], uint32(length))
	binary.BigEndian.PutUint32(h[32:], 0xffffffff) // not ISO
	copy(h[40:], setup[:])
	c.dirs[c.seq] = dir
	c.write(append(h, data...))
	return c.seq
}

func (c *client) control(setup [8]byte, data []byte) uint32 {
	dir := DirOut
	if setup[0]&0x80 != 0 {
		dir = DirIn
	}
	length := int(binary.LittleEndian.Uint16(setup[6:]))
	return c.submit(dir, 0, length, setup, data)
}

func (c *client) unlink(victim uint32) uint32 {
	c.t.Helper()
	c.seq++
	h := make([]byte, headerSize)
	binary.BigEndian.PutUint32(h[0:], CmdUnlink)
	binary.BigEndian.PutUint32(h[4:], c.seq)
	binary.BigEndian.PutUint32(h[20:], victim)
	c.write(h)
	return c.seq
}

type ret struct {
	cmd    uint32
	seq    uint32
	status int32
	data   []byte
}

func (c *client) ret() ret {
	c.t.Helper()
	h := c.read(headerSize)
	r := ret{cmd: binary.BigEndian.Uint32(h[0:]), seq: binary.BigEndian.Uint32(h[4:]), status: int32(binary.BigEndian.Uint32(h[20:]))}
	if r.cmd == RetSubmit && c.dirs[r.seq] == DirIn {
		if n := int(binary.BigEndian.Uint32(h[24:])); n > 0 {
			r.data = c.read(n)
		}
	}
	return r
}

func setup(rt, req byte, value, index, length uint16) [8]byte {
	var s [8]byte
	s[0], s[1] = rt, req
	binary.LittleEndian.PutUint16(s[2:], value)
	binary.LittleEndian.PutUint16(s[4:], index)
	binary.LittleEndian.PutUint16(s[6:], length)
	return s
}

func startServer(t *testing.T) (*ACM, string) {
	t.Helper()
	dev := &ACM{VendorID: 0x1209, ProductID: 0x0001, Manufacturer: "test", Product: "virtual port", Serial: "T1", BusID: "1-1", Logf: t.Logf}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Device: dev, Logf: t.Logf}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln)
	return dev, ln.Addr().String()
}

func TestDevlistAndImport(t *testing.T) {
	_, addr := startServer(t)

	c := newClient(t, addr)
	c.write(encodeOpHeader(OpReqDevlist, 0))
	rep := c.read(opHeaderSize + 4)
	if binary.BigEndian.Uint16(rep[2:]) != OpRepDevlist || binary.BigEndian.Uint32(rep[8:]) != 1 {
		t.Fatalf("devlist reply % X", rep)
	}
	dev := c.read(312 + 2*4)
	if got := string(trimNul(dev[256:288])); got != "1-1" {
		t.Fatalf("busid %q", got)
	}
	if dev[312] != 0x02 || dev[316] != 0x0A {
		t.Fatalf("interface classes %#x %#x", dev[312], dev[316])
	}
	c.nc.Close()

	bad := newClient(t, addr)
	if bad.importDevice("9-9") != nil {
		t.Fatal("import of unknown busid accepted")
	}
	bad.nc.Close()

	c = newClient(t, addr)
	info := c.importDevice("1-1")
	if info == nil {
		t.Fatal("import refused")
	}
	if vid := binary.BigEndian.Uint16(info[300:]); vid != 0x1209 {
		t.Fatalf("vendor id %#x", vid)
	}
	second := newClient(t, addr)
	if second.importDevice("1-1") != nil {
		t.Fatal("second import accepted while device in use")
	}
}

func TestEnumerationAndSerialPath(t *testing.T) {
	dev, addr := startServer(t)
	c := newClient(t, addr)
	if c.importDevice("1-1") == nil {
		t.Fatal("import refused")
	}

	// Device descriptor, first 8 bytes then all 18 bytes (as hosts do).
	c.control(setup(0x80, 0x06, 0x0100, 0, 8), nil)
	if r := c.ret(); r.status != 0 || len(r.data) != 8 || r.data[7] != maxPacket {
		t.Fatalf("device descriptor (8): %+v", r)
	}
	c.control(setup(0x80, 0x06, 0x0100, 0, 18), nil)
	r := c.ret()
	if r.status != 0 || len(r.data) != 18 || binary.LittleEndian.Uint16(r.data[8:]) != 0x1209 {
		t.Fatalf("device descriptor: %+v", r)
	}
	// Configuration descriptor: header first, then the full length it announces.
	c.control(setup(0x80, 0x06, 0x0200, 0, 9), nil)
	r = c.ret()
	total := int(binary.LittleEndian.Uint16(r.data[2:]))
	c.control(setup(0x80, 0x06, 0x0200, 0, uint16(total)), nil)
	r = c.ret()
	if len(r.data) != total {
		t.Fatalf("config descriptor length %d, want %d", len(r.data), total)
	}
	for at := 0; at < total; at += int(r.data[at]) { // every bLength lands on the next descriptor
		if r.data[at] == 0 {
			t.Fatalf("zero-length descriptor at %d", at)
		}
	}
	// Serial number string and an unsupported descriptor (device qualifier → STALL).
	c.control(setup(0x80, 0x06, 0x0303, 0x0409, 255), nil)
	if r = c.ret(); r.status != 0 || !bytes.Equal(r.data, []byte{6, 3, 'T', 0, '1', 0}) {
		t.Fatalf("serial string: %+v", r)
	}
	c.control(setup(0x80, 0x06, 0x0600, 0, 10), nil)
	if r = c.ret(); r.status != StatusEPIPE {
		t.Fatalf("device qualifier should stall: %+v", r)
	}
	// SET_CONFIGURATION, then the driver starts the notification pipe and gets SERIAL_STATE once.
	c.control(setup(0x00, 0x09, 1, 0, 0), nil)
	if r = c.ret(); r.status != 0 {
		t.Fatalf("set configuration: %+v", r)
	}
	notifySeq := c.submit(DirIn, epNotify, 16, [8]byte{}, nil)
	if r = c.ret(); r.seq != notifySeq || r.status != 0 || !bytes.Equal(r.data, serialStateNotification()) {
		t.Fatalf("serial state notification: %+v", r)
	}
	notify2 := c.submit(DirIn, epNotify, 16, [8]byte{}, nil) // stays pending
	// Line coding round trip and DTR.
	c.control(setup(0x21, 0x20, 0, 0, 7), []byte{0x80, 0x25, 0, 0, 2, 0, 8}) // 9600, 2 stop bits, none, 8
	if r = c.ret(); r.status != 0 {
		t.Fatalf("set line coding: %+v", r)
	}
	c.control(setup(0xA1, 0x21, 0, 0, 7), nil)
	if r = c.ret(); !bytes.Equal(r.data, []byte{0x80, 0x25, 0, 0, 2, 0, 8}) {
		t.Fatalf("get line coding: %+v", r)
	}
	c.control(setup(0x21, 0x22, 0x03, 0, 0), nil)
	c.ret()
	if st := dev.State(); !st.Configured || !st.DTR || st.Line.StopBits != 2 {
		t.Fatalf("state %+v", st)
	}

	// Application writes to the COM port: bulk OUT arrives in Read.
	readCh := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := dev.Read(buf)
		readCh <- buf[:n]
	}()
	c.submit(DirOut, epDataOut, 3, [8]byte{}, []byte{0x10, 0x02, 0x06})
	if r = c.ret(); r.status != 0 {
		t.Fatalf("bulk out: %+v", r)
	}
	select {
	case got := <-readCh:
		if !bytes.Equal(got, []byte{0x10, 0x02, 0x06}) {
			t.Fatalf("Read got % X", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return")
	}

	// Application reads: a bulk IN stays pending until the device writes; the port counts as open.
	inSeq := c.submit(DirIn, epDataIn, 4096, [8]byte{}, nil)
	time.Sleep(50 * time.Millisecond)
	if !dev.State().Open {
		t.Fatal("port should be open while a read is pending")
	}
	if _, err := dev.Write([]byte{0x06}); err != nil {
		t.Fatal(err)
	}
	if r = c.ret(); r.seq != inSeq || !bytes.Equal(r.data, []byte{0x06}) {
		t.Fatalf("bulk in: %+v", r)
	}
	// Data written while no read is pending is delivered with the next read, chunked to its length.
	dev.Write([]byte{1, 2, 3, 4, 5})
	in2 := c.submit(DirIn, epDataIn, 2, [8]byte{}, nil)
	if r = c.ret(); r.seq != in2 || !bytes.Equal(r.data, []byte{1, 2}) {
		t.Fatalf("chunked bulk in: %+v", r)
	}
	in3 := c.submit(DirIn, epDataIn, 64, [8]byte{}, nil)
	if r = c.ret(); r.seq != in3 || !bytes.Equal(r.data, []byte{3, 4, 5}) {
		t.Fatalf("rest of bulk in: %+v", r)
	}

	// Application closes the port: the host unlinks its read and the notification pipe.
	in4 := c.submit(DirIn, epDataIn, 64, [8]byte{}, nil)
	u := c.unlink(in4)
	if r = c.ret(); r.cmd != RetUnlink || r.seq != u || r.status != StatusECONNRESET {
		t.Fatalf("unlink pending read: %+v", r)
	}
	u = c.unlink(notify2)
	if r = c.ret(); r.status != StatusECONNRESET {
		t.Fatalf("unlink notification: %+v", r)
	}
	if dev.State().Open {
		t.Fatal("port should be closed after both unlinks")
	}
	u = c.unlink(9999) // nothing pending with that seqnum
	if r = c.ret(); r.status != 0 {
		t.Fatalf("unlink of completed urb: %+v", r)
	}
	// Reopen: notification is delivered again.
	n3 := c.submit(DirIn, epNotify, 16, [8]byte{}, nil)
	if r = c.ret(); r.seq != n3 || !bytes.Equal(r.data, serialStateNotification()) {
		t.Fatalf("notification after reopen: %+v", r)
	}
	// Unknown endpoint stalls.
	c.submit(DirIn, 5, 8, [8]byte{}, nil)
	if r = c.ret(); r.status != StatusEPIPE {
		t.Fatalf("unknown endpoint: %+v", r)
	}

	// Client disconnects: device detached, writes are dropped, Read still serves buffered data.
	c.nc.Close()
	deadline := time.Now().Add(2 * time.Second)
	for dev.State().Attached && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if dev.State().Attached {
		t.Fatal("device still attached after disconnect")
	}
	if n, err := dev.Write([]byte{1}); err != nil || n != 1 {
		t.Fatalf("write while detached: %d %v", n, err)
	}
}
