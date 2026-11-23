// Package usbip implements the server side of the USB/IP protocol
// (https://www.kernel.org/doc/html/latest/usb/usbip_protocol.html) so that a
// USB device emulated in software can be plugged into a USB/IP client: the
// Linux vhci-hcd driver or the usbip-win2 virtual host controller on Windows.
package usbip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Version uint16 = 0x0111

	OpReqDevlist uint16 = 0x8005
	OpRepDevlist uint16 = 0x0005
	OpReqImport  uint16 = 0x8003
	OpRepImport  uint16 = 0x0003

	CmdSubmit uint32 = 1
	CmdUnlink uint32 = 2
	RetSubmit uint32 = 3
	RetUnlink uint32 = 4

	DirOut uint32 = 0
	DirIn  uint32 = 1

	// Linux errno values used as URB status.
	StatusOK         int32 = 0
	StatusENOENT     int32 = -2
	StatusENODEV     int32 = -19
	StatusEPIPE      int32 = -32 // STALL
	StatusEOVERFLOW  int32 = -75
	StatusECONNRESET int32 = -104 // URB unlinked

	// enum usb_device_speed
	SpeedFull uint32 = 2
	SpeedHigh uint32 = 3

	headerSize   = 48
	maxTransfer  = 1 << 20
	opHeaderSize = 8
)

// ErrStall is returned by Device.Control for unsupported requests.
var ErrStall = errors.New("usb: request stalled")

// InterfaceInfo describes one interface for the device list.
type InterfaceInfo struct{ Class, SubClass, Protocol uint8 }

// DeviceInfo is what the client learns about the device before enumeration.
type DeviceInfo struct {
	Path       string // informational, e.g. /sys/devices/virtual/usb/1-1
	BusID      string // e.g. 1-1; the client imports the device by this id
	BusNum     uint32
	DevNum     uint32
	Speed      uint32
	VendorID   uint16
	ProductID  uint16
	BCDDevice  uint16
	Class      uint8
	SubClass   uint8
	Protocol   uint8
	ConfigVal  uint8
	NumConfigs uint8
	Interfaces []InterfaceInfo
}

// DevID is the device identifier the client uses in URB headers.
func (d DeviceInfo) DevID() uint32 { return d.BusNum<<16 | d.DevNum }

func putFixedString(b []byte, s string) {
	copy(b, s)
	if len(s) < len(b) {
		b[len(s)] = 0
	}
}

func (d DeviceInfo) encode(withInterfaces bool) []byte {
	out := make([]byte, 0, 312+4*len(d.Interfaces))
	path := make([]byte, 256)
	putFixedString(path, d.Path)
	busid := make([]byte, 32)
	putFixedString(busid, d.BusID)
	out = append(out, path...)
	out = append(out, busid...)
	out = binary.BigEndian.AppendUint32(out, d.BusNum)
	out = binary.BigEndian.AppendUint32(out, d.DevNum)
	out = binary.BigEndian.AppendUint32(out, d.Speed)
	out = binary.BigEndian.AppendUint16(out, d.VendorID)
	out = binary.BigEndian.AppendUint16(out, d.ProductID)
	out = binary.BigEndian.AppendUint16(out, d.BCDDevice)
	out = append(out, d.Class, d.SubClass, d.Protocol, d.ConfigVal, d.NumConfigs, uint8(len(d.Interfaces)))
	if withInterfaces {
		for _, i := range d.Interfaces {
			out = append(out, i.Class, i.SubClass, i.Protocol, 0)
		}
	}
	return out
}

func encodeOpHeader(code uint16, status uint32) []byte {
	out := make([]byte, 0, opHeaderSize)
	out = binary.BigEndian.AppendUint16(out, Version)
	out = binary.BigEndian.AppendUint16(out, code)
	return binary.BigEndian.AppendUint32(out, status)
}

// header is the common 48-byte URB header.
type header struct {
	Command   uint32
	Seq       uint32
	DevID     uint32
	Direction uint32
	EP        uint32
	// CMD_SUBMIT
	TransferFlags  uint32
	TransferLength int32
	StartFrame     int32
	NumPackets     int32
	Interval       int32
	Setup          [8]byte
	// CMD_UNLINK
	UnlinkSeq uint32
}

func readHeader(r io.Reader) (*header, error) {
	var b [headerSize]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return nil, err
	}
	h := &header{
		Command:   binary.BigEndian.Uint32(b[0:]),
		Seq:       binary.BigEndian.Uint32(b[4:]),
		DevID:     binary.BigEndian.Uint32(b[8:]),
		Direction: binary.BigEndian.Uint32(b[12:]),
		EP:        binary.BigEndian.Uint32(b[16:]),
	}
	switch h.Command {
	case CmdSubmit:
		h.TransferFlags = binary.BigEndian.Uint32(b[20:])
		h.TransferLength = int32(binary.BigEndian.Uint32(b[24:]))
		h.StartFrame = int32(binary.BigEndian.Uint32(b[28:]))
		h.NumPackets = int32(binary.BigEndian.Uint32(b[32:]))
		h.Interval = int32(binary.BigEndian.Uint32(b[36:]))
		copy(h.Setup[:], b[40:48])
	case CmdUnlink:
		h.UnlinkSeq = binary.BigEndian.Uint32(b[20:])
	default:
		return nil, fmt.Errorf("usbip: unknown command %#x", h.Command)
	}
	return h, nil
}

// encodeRetSubmit builds USBIP_RET_SUBMIT. For IN transfers data follows the header.
func encodeRetSubmit(seq uint32, status int32, data []byte) []byte {
	out := make([]byte, headerSize, headerSize+len(data))
	binary.BigEndian.PutUint32(out[0:], RetSubmit)
	binary.BigEndian.PutUint32(out[4:], seq)
	binary.BigEndian.PutUint32(out[20:], uint32(status))
	binary.BigEndian.PutUint32(out[24:], uint32(len(data)))
	return append(out, data...)
}

// encodeRetSubmitOut builds USBIP_RET_SUBMIT for an OUT transfer that moved n bytes.
func encodeRetSubmitOut(seq uint32, status int32, n int) []byte {
	out := make([]byte, headerSize)
	binary.BigEndian.PutUint32(out[0:], RetSubmit)
	binary.BigEndian.PutUint32(out[4:], seq)
	binary.BigEndian.PutUint32(out[20:], uint32(status))
	binary.BigEndian.PutUint32(out[24:], uint32(n))
	return out
}

func encodeRetUnlink(seq uint32, status int32) []byte {
	out := make([]byte, headerSize)
	binary.BigEndian.PutUint32(out[0:], RetUnlink)
	binary.BigEndian.PutUint32(out[4:], seq)
	binary.BigEndian.PutUint32(out[20:], uint32(status))
	return out
}
