package usbip

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"unicode/utf16"
)

// Endpoint numbers of the CDC-ACM device (without direction bit).
const (
	epDataOut = 1 // bulk OUT 0x01: host → device
	epDataIn  = 2 // bulk IN 0x82: device → host
	epNotify  = 3 // interrupt IN 0x83: SERIAL_STATE notifications
	maxPacket = 64
)

// LineCoding is the CDC line coding the host last set. It is informational:
// the data path does not change with it.
type LineCoding struct {
	BaudRate uint32
	StopBits uint8 // 0 = 1, 1 = 1.5, 2 = 2
	Parity   uint8 // 0 none, 1 odd, 2 even, 3 mark, 4 space
	DataBits uint8
}

// ACMState is a snapshot of the virtual port.
type ACMState struct {
	Attached   bool // a USB/IP client has the device plugged in
	Configured bool // the host set a configuration (driver loaded)
	Open       bool // the host is reading, i.e. an application has the COM port open
	DTR, RTS   bool
	Line       LineCoding
}

// ACM is a USB CDC-ACM ("USB serial") device emulated in software. Windows
// binds its inbox usbser.sys to it and creates a COM port; Linux creates
// /dev/ttyACMn. The device end is an io.ReadWriteCloser: Read returns what the
// application wrote to the COM port, Write delivers bytes to the application.
type ACM struct {
	VendorID     uint16
	ProductID    uint16
	Manufacturer string
	Product      string
	Serial       string // iSerialNumber; keep it fixed so the host reuses the same COM number
	BusID        string // USB/IP bus id, e.g. 1-1
	Logf         func(format string, args ...any)
	// OnState is called when Attached/Configured/Open/DTR change (optional).
	OnState func(ACMState)
	// Trace logs every control request with its answer (enumeration debugging).
	Trace bool

	mu         sync.Mutex
	cond       *sync.Cond
	attached   bool
	configured bool
	line       LineCoding
	dtr, rts   bool
	notified   bool
	closed     bool
	fromHost   []byte     // received on bulk OUT, waiting for Read
	toHost     []byte     // queued for bulk IN
	pendIn     []*Request // bulk IN transfers waiting for data
	pendNotify []*Request // interrupt IN transfers waiting for a notification
	reading    bool       // the host has had a bulk IN outstanding since the port was opened
}

const (
	fromHostLimit = 1 << 20
	toHostLimit   = 1 << 20
)

func (a *ACM) init() {
	if a.cond == nil {
		a.cond = sync.NewCond(&a.mu)
	}
	if a.line.BaudRate == 0 {
		a.line = LineCoding{BaudRate: 9600, DataBits: 8}
	}
}

func (a *ACM) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// Info implements Device.
func (a *ACM) Info() DeviceInfo {
	return DeviceInfo{
		Path:       "/sys/devices/virtual/usbip/" + a.BusID,
		BusID:      a.BusID,
		BusNum:     1,
		DevNum:     2,
		Speed:      SpeedFull,
		VendorID:   a.VendorID,
		ProductID:  a.ProductID,
		BCDDevice:  0x0100,
		Class:      0xEF, // Miscellaneous
		SubClass:   0x02, // Common
		Protocol:   0x01, // Interface Association Descriptor
		ConfigVal:  1,
		NumConfigs: 1,
		Interfaces: []InterfaceInfo{{0x02, 0x02, 0x01}, {0x0A, 0x00, 0x00}},
	}
}

func (a *ACM) deviceDescriptor() []byte {
	d := []byte{
		18, 0x01, // bLength, DEVICE
		0x00, 0x02, // bcdUSB 2.00
		0xEF, 0x02, 0x01, // Miscellaneous / Common / IAD
		maxPacket,
		0, 0, 0, 0, 0x00, 0x01, // idVendor, idProduct, bcdDevice (patched below)
		1, 2, 3, // iManufacturer, iProduct, iSerialNumber
		1, // bNumConfigurations
	}
	binary.LittleEndian.PutUint16(d[8:], a.VendorID)
	binary.LittleEndian.PutUint16(d[10:], a.ProductID)
	return d
}

func configDescriptor() []byte {
	d := []byte{
		// Configuration: 2 interfaces, value 1, bus powered, 100 mA. wTotalLength patched below.
		9, 0x02, 0, 0, 2, 1, 0, 0x80, 50,
		// Interface association: interfaces 0-1 form one CDC-ACM function.
		8, 0x0B, 0, 2, 0x02, 0x02, 0x01, 0,
		// Interface 0: communication class, ACM, AT commands, one endpoint.
		9, 0x04, 0, 0, 1, 0x02, 0x02, 0x01, 0,
		5, 0x24, 0x00, 0x10, 0x01, // header functional descriptor, CDC 1.10
		5, 0x24, 0x01, 0x00, 0x01, // call management: data interface 1
		4, 0x24, 0x02, 0x02, // ACM: line coding and control line state
		5, 0x24, 0x06, 0x00, 0x01, // union: master 0, slave 1
		7, 0x05, 0x80 | epNotify, 0x03, 16, 0, 16, // interrupt IN, 16 bytes, 16 ms
		// Interface 1: data class, two bulk endpoints.
		9, 0x04, 1, 0, 2, 0x0A, 0x00, 0x00, 0,
		7, 0x05, epDataOut, 0x02, maxPacket, 0, 0, // bulk OUT
		7, 0x05, 0x80 | epDataIn, 0x02, maxPacket, 0, 0, // bulk IN
	}
	binary.LittleEndian.PutUint16(d[2:], uint16(len(d)))
	return d
}

func stringDescriptor(s string) []byte {
	u := utf16.Encode([]rune(s))
	d := make([]byte, 2, 2+2*len(u))
	for _, c := range u {
		d = binary.LittleEndian.AppendUint16(d, c)
	}
	d[0], d[1] = byte(len(d)), 0x03
	return d
}

// Control implements Device.
func (a *ACM) Control(setup [8]byte, data []byte) ([]byte, error) {
	reply, err := a.control(setup, data)
	if a.Trace {
		switch {
		case err != nil:
			a.logf("usb: control % X → STALL", setup)
		case setup[0]&0x80 != 0:
			a.logf("usb: control % X → %d B % X", setup, len(reply), reply)
		default:
			a.logf("usb: control % X (+%d B data) → ok", setup, len(data))
		}
	}
	return reply, err
}

func (a *ACM) control(setup [8]byte, data []byte) ([]byte, error) {
	a.init()
	reqType, req := setup[0], setup[1]
	value := binary.LittleEndian.Uint16(setup[2:])
	length := int(binary.LittleEndian.Uint16(setup[6:]))
	reply := func(b []byte) ([]byte, error) {
		if len(b) > length {
			b = b[:length]
		}
		return b, nil
	}
	switch {
	case reqType == 0x80 && req == 0x06: // GET_DESCRIPTOR
		index, kind := byte(value), byte(value>>8)
		switch kind {
		case 0x01:
			return reply(a.deviceDescriptor())
		case 0x02:
			return reply(configDescriptor())
		case 0x03:
			switch index {
			case 0:
				return reply([]byte{4, 0x03, 0x09, 0x04}) // English (US)
			case 1:
				return reply(stringDescriptor(a.Manufacturer))
			case 2:
				return reply(stringDescriptor(a.Product))
			case 3:
				return reply(stringDescriptor(a.Serial))
			}
		}
		return nil, ErrStall // device qualifier, BOS, OS descriptors: not offered
	case reqType >= 0x80 && reqType <= 0x82 && req == 0x00: // GET_STATUS
		return reply([]byte{0, 0})
	case reqType == 0x80 && req == 0x08: // GET_CONFIGURATION
		a.mu.Lock()
		c := a.configured
		a.mu.Unlock()
		if c {
			return reply([]byte{1})
		}
		return reply([]byte{0})
	case reqType == 0x00 && req == 0x09: // SET_CONFIGURATION
		a.mu.Lock()
		a.configured = value != 0
		a.notified = false
		a.mu.Unlock()
		a.logf("usb: configured=%v", value != 0)
		a.changed()
		a.deliverNotification()
		return nil, nil
	case (reqType == 0x00 && req == 0x05) || // SET_ADDRESS
		(reqType <= 0x02 && (req == 0x01 || req == 0x03)) || // CLEAR_FEATURE, SET_FEATURE
		(reqType == 0x01 && req == 0x0B): // SET_INTERFACE
		return nil, nil
	case reqType == 0x81 && req == 0x0A: // GET_INTERFACE
		return reply([]byte{0})
	case reqType == 0x21 && req == 0x20: // CDC SET_LINE_CODING
		if len(data) < 7 {
			return nil, ErrStall
		}
		a.mu.Lock()
		a.line = LineCoding{BaudRate: binary.LittleEndian.Uint32(data), StopBits: data[4], Parity: data[5], DataBits: data[6]}
		l := a.line
		a.mu.Unlock()
		a.logf("usb: line coding %d baud, %d data bits, parity %d, stop bits code %d", l.BaudRate, l.DataBits, l.Parity, l.StopBits)
		a.changed()
		return nil, nil
	case reqType == 0xA1 && req == 0x21: // CDC GET_LINE_CODING
		a.mu.Lock()
		l := a.line
		a.mu.Unlock()
		b := binary.LittleEndian.AppendUint32(nil, l.BaudRate)
		return reply(append(b, l.StopBits, l.Parity, l.DataBits))
	case reqType == 0x21 && req == 0x22: // CDC SET_CONTROL_LINE_STATE
		a.mu.Lock()
		a.dtr, a.rts = value&1 != 0, value&2 != 0
		a.mu.Unlock()
		a.logf("usb: DTR=%v RTS=%v", value&1 != 0, value&2 != 0)
		a.changed()
		return nil, nil
	case reqType == 0x21 && req == 0x23: // CDC SEND_BREAK
		return nil, nil
	}
	return nil, ErrStall
}

// serialStateNotification reports DCD and DSR asserted: a "cable" is attached.
func serialStateNotification() []byte {
	return []byte{0xA1, 0x20, 0, 0, 0, 0, 2, 0, 0x03, 0x00}
}

// Submit implements Device.
func (a *ACM) Submit(req *Request) {
	a.init()
	switch {
	case req.EP == epDataOut && !req.In:
		a.mu.Lock()
		if len(a.fromHost)+len(req.Data) > fromHostLimit {
			a.logf("usb: receive buffer full, %d bytes dropped", len(req.Data))
		} else {
			a.fromHost = append(a.fromHost, req.Data...)
		}
		a.cond.Broadcast()
		a.mu.Unlock()
		req.Complete(StatusOK, nil)
	case req.EP == epDataIn && req.In:
		a.mu.Lock()
		wasReading := a.reading
		a.reading = true
		if len(a.toHost) > 0 {
			n := min(len(a.toHost), req.Length)
			chunk := a.toHost[:n:n]
			a.toHost = a.toHost[n:]
			a.mu.Unlock()
			req.Complete(StatusOK, chunk)
		} else {
			a.pendIn = append(a.pendIn, req)
			a.mu.Unlock()
		}
		if !wasReading {
			a.logf("usb: port opened by the host application")
			a.changed()
		}
	case req.EP == epNotify && req.In:
		a.mu.Lock()
		a.pendNotify = append(a.pendNotify, req)
		a.mu.Unlock()
		a.deliverNotification()
	default:
		req.Complete(StatusEPIPE, nil)
	}
}

// deliverNotification completes one pending interrupt transfer with the
// SERIAL_STATE notification, once per configuration.
func (a *ACM) deliverNotification() {
	a.mu.Lock()
	if !a.configured || a.notified || len(a.pendNotify) == 0 {
		a.mu.Unlock()
		return
	}
	req := a.pendNotify[0]
	a.pendNotify = a.pendNotify[1:]
	a.notified = true
	a.mu.Unlock()
	req.Complete(StatusOK, serialStateNotification())
}

// Unlink implements Device.
func (a *ACM) Unlink(seq uint32) bool {
	a.init()
	a.mu.Lock()
	found := false
	for i, r := range a.pendIn {
		if r.Seq == seq {
			a.pendIn = append(a.pendIn[:i], a.pendIn[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		for i, r := range a.pendNotify {
			if r.Seq == seq {
				a.pendNotify = append(a.pendNotify[:i], a.pendNotify[i+1:]...)
				found = true
				break
			}
		}
	}
	// usbser.sys cancels its read and the notification pipe when the application closes the port.
	closed := a.reading && len(a.pendIn) == 0 && len(a.pendNotify) == 0
	if closed {
		a.reading = false
		a.notified = false
	}
	a.mu.Unlock()
	if closed {
		a.logf("usb: port closed by the host application")
		a.changed()
	}
	return found
}

// Attached implements Device.
func (a *ACM) Attached() {
	a.init()
	a.mu.Lock()
	a.attached = true
	a.mu.Unlock()
	a.changed()
}

// Detached implements Device: the client is gone, pending transfers are void.
func (a *ACM) Detached() {
	a.init()
	a.mu.Lock()
	a.attached, a.configured, a.notified, a.reading = false, false, false, false
	a.pendIn, a.pendNotify, a.toHost = nil, nil, nil
	a.mu.Unlock()
	a.changed()
}

// State returns a snapshot.
func (a *ACM) State() ACMState {
	a.init()
	a.mu.Lock()
	defer a.mu.Unlock()
	return ACMState{Attached: a.attached, Configured: a.configured, Open: a.reading, DTR: a.dtr, RTS: a.rts, Line: a.line}
}

func (a *ACM) changed() {
	if a.OnState != nil {
		a.OnState(a.State())
	}
}

// Read returns bytes the host application wrote to the COM port. It blocks
// until data is available or the device is closed.
func (a *ACM) Read(p []byte) (int, error) {
	a.init()
	a.mu.Lock()
	defer a.mu.Unlock()
	for len(a.fromHost) == 0 && !a.closed {
		a.cond.Wait()
	}
	if a.closed && len(a.fromHost) == 0 {
		return 0, io.EOF
	}
	n := copy(p, a.fromHost)
	a.fromHost = a.fromHost[n:]
	return n, nil
}

// Write delivers bytes to the host application. Data is queued until the host
// reads; without an attached host it is discarded like on an unplugged cable.
func (a *ACM) Write(p []byte) (int, error) {
	a.init()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return 0, errors.New("usb: device closed")
	}
	if !a.attached {
		a.mu.Unlock()
		a.logf("usb: no host attached, %d bytes dropped", len(p))
		return len(p), nil
	}
	if len(a.toHost)+len(p) > toHostLimit {
		a.mu.Unlock()
		a.logf("usb: transmit buffer full, %d bytes dropped", len(p))
		return len(p), nil
	}
	a.toHost = append(a.toHost, p...)
	var completions []struct {
		req  *Request
		data []byte
	}
	for len(a.pendIn) > 0 && len(a.toHost) > 0 {
		req := a.pendIn[0]
		a.pendIn = a.pendIn[1:]
		n := min(len(a.toHost), req.Length)
		completions = append(completions, struct {
			req  *Request
			data []byte
		}{req, a.toHost[:n:n]})
		a.toHost = a.toHost[n:]
	}
	a.mu.Unlock()
	for _, c := range completions {
		c.req.Complete(StatusOK, c.data)
	}
	return len(p), nil
}

// Close wakes blocked readers; the device stays enumerable but discards data.
func (a *ACM) Close() error {
	a.init()
	a.mu.Lock()
	a.closed = true
	a.cond.Broadcast()
	a.mu.Unlock()
	return nil
}
