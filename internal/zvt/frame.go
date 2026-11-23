// Package zvt implements the transport layers of the ZVT ECR interface
// (the German "ZVT-Kassenschnittstelle" published by Verband der
// Terminalhersteller e.V.): the serial framing with CRC and ACK/NAK, and the
// unframed APDU stream used over TCP/IP.
package zvt

import (
	"fmt"
	"io"
	"strings"
)

// Control characters of the serial ZVT transport layer.
const (
	STX byte = 0x02
	ETX byte = 0x03
	ACK byte = 0x06
	DLE byte = 0x10
	NAK byte = 0x15
)

var crcTable [256]uint16

func init() {
	for i := 0; i < 256; i++ {
		c := uint16(i)
		for j := 0; j < 8; j++ {
			if c&1 == 1 {
				c = (c >> 1) ^ 0x8408
			} else {
				c >>= 1
			}
		}
		crcTable[i] = c
	}
}

// CRC16 computes the ZVT checksum over the APDU plus ETX
// (polynomial 0x8408, initial value 0). On the wire the low byte is
// transmitted first, then the high byte.
func CRC16(apdu []byte) uint16 {
	var c uint16
	update := func(b byte) {
		hb := c >> 8
		lb := byte(c & 0xFF)
		c = crcTable[lb^b] ^ hb
	}
	for _, b := range apdu {
		update(b)
	}
	update(ETX)
	return c
}

// EncodeFrame wraps an APDU into a serial frame:
// DLE STX <APDU with DLE doubled> DLE ETX CRC-L CRC-H
func EncodeFrame(apdu []byte) []byte {
	out := make([]byte, 0, len(apdu)+8)
	out = append(out, DLE, STX)
	for _, b := range apdu {
		out = append(out, b)
		if b == DLE {
			out = append(out, DLE)
		}
	}
	c := CRC16(apdu)
	return append(out, DLE, ETX, byte(c&0xFF), byte(c>>8))
}

// EventKind classifies what the Decoder produced from the byte stream.
type EventKind int

const (
	EvNone     EventKind = iota
	EvAck                // bare ACK outside a frame
	EvNak                // bare NAK outside a frame
	EvFrame              // complete frame, CRC valid
	EvBadCRC             // complete frame, CRC invalid
	EvGarbage            // byte outside a frame that is neither DLE nor ACK/NAK
	EvProtocol           // DLE followed by an unexpected byte
)

// Event is one decoded item from the serial byte stream.
type Event struct {
	Kind EventKind
	APDU []byte
	Byte byte
}

const (
	sIdle = iota
	sDLE
	sData
	sDataDLE
	sCRC1
	sCRC2
)

// Decoder is the byte-wise state machine for the serial side.
type Decoder struct {
	state int
	buf   []byte
	crcLo byte
}

// Busy reports whether the decoder is in the middle of a frame.
func (d *Decoder) Busy() bool { return d.state != sIdle }

// Reset discards a partially received frame.
func (d *Decoder) Reset() {
	d.state = sIdle
	d.buf = d.buf[:0]
}

// Feed consumes one byte and returns an event (Kind EvNone if nothing completed).
func (d *Decoder) Feed(b byte) Event {
	switch d.state {
	case sIdle:
		switch b {
		case DLE:
			d.state = sDLE
		case ACK:
			return Event{Kind: EvAck}
		case NAK:
			return Event{Kind: EvNak}
		default:
			return Event{Kind: EvGarbage, Byte: b}
		}
	case sDLE:
		if b == STX {
			d.state = sData
			d.buf = d.buf[:0]
		} else {
			d.state = sIdle
			return Event{Kind: EvProtocol, Byte: b}
		}
	case sData:
		if b == DLE {
			d.state = sDataDLE
		} else {
			d.buf = append(d.buf, b)
		}
	case sDataDLE:
		switch b {
		case DLE:
			d.buf = append(d.buf, DLE)
			d.state = sData
		case ETX:
			d.state = sCRC1
		default:
			d.state = sIdle
			return Event{Kind: EvProtocol, Byte: b}
		}
	case sCRC1:
		d.crcLo = b
		d.state = sCRC2
	case sCRC2:
		d.state = sIdle
		got := uint16(d.crcLo) | uint16(b)<<8
		apdu := append([]byte(nil), d.buf...)
		if got == CRC16(apdu) {
			return Event{Kind: EvFrame, APDU: apdu}
		}
		return Event{Kind: EvBadCRC, APDU: apdu}
	}
	return Event{}
}

// ReadAPDU reads one complete APDU from a TCP stream:
// CCRC APRC LEN [LL HH if LEN == 0xFF] DATA
func ReadAPDU(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 3)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	n := int(hdr[2])
	apdu := hdr
	if n == 0xFF {
		ext := make([]byte, 2)
		if _, err := io.ReadFull(r, ext); err != nil {
			return nil, err
		}
		n = int(ext[0]) | int(ext[1])<<8
		apdu = append(apdu, ext...)
	}
	if n > 0 {
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		apdu = append(apdu, data...)
	}
	return apdu, nil
}

var apduNames = map[uint16]string{
	0x0501: "Status-Enquiry",
	0x0600: "Registration",
	0x0601: "Authorization",
	0x0602: "Log-Off",
	0x060F: "Completion",
	0x061E: "Abort",
	0x0620: "Repeat-Receipt",
	0x0630: "Reversal",
	0x0650: "End-of-Day",
	0x0670: "Display-Text",
	0x0685: "Selection",
	0x0693: "Initialisation",
	0x06B0: "Abort (ECR)",
	0x06C0: "Read-Card",
	0x06D1: "Print-Line",
	0x06D3: "Print-Text-Block",
	0x0801: "Diagnosis",
	0x040F: "Status-Information",
	0x04FF: "Intermediate-Status",
	0x8000: "ACK (positive)",
	0x8400: "ACK (84 00)",
}

// APDUName returns a human-readable name for log output.
func APDUName(apdu []byte) string {
	if len(apdu) < 2 {
		return "?"
	}
	key := uint16(apdu[0])<<8 | uint16(apdu[1])
	if n, ok := apduNames[key]; ok {
		return n
	}
	if apdu[0] == 0x84 {
		return fmt.Sprintf("Error 84 %02X", apdu[1])
	}
	return fmt.Sprintf("%02X %02X", apdu[0], apdu[1])
}

// HexDump formats bytes as hex groups, truncated after max bytes (0 = no limit).
func HexDump(b []byte, max int) string {
	var sb strings.Builder
	for i, x := range b {
		if max > 0 && i >= max {
			fmt.Fprintf(&sb, " … (+%d)", len(b)-max)
			break
		}
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02X", x)
	}
	return sb.String()
}
