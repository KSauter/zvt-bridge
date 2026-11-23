package zvt

import (
	"bytes"
	"testing"
)

// Vectors computed with the reference algorithm from ecrterm (Python).
var crcVectors = []struct {
	name string
	APDU []byte
	lo   byte
	hi   byte
}{
	{"Registration", []byte{0x06, 0x00, 0x06, 0x12, 0x34, 0x56, 0x78, 0x09, 0x78}, 0x8b, 0xc1},
	{"ACK 80 00 00", []byte{0x80, 0x00, 0x00}, 0xf5, 0x1f},
	{"Authorization", []byte{0x06, 0x01, 0x07, 0x04, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x09, 0x19}, 0x89, 0x6f},
	{"with DLE", []byte{0x06, 0xd1, 0x05, 0x10, 0x06, 0x10, 0x10, 0x03}, 0x87, 0x07},
	{"Status-Info", []byte{0x04, 0x0f, 0x01, 0x27, 0x00, 0x0a}, 0x22, 0x4e},
}

func TestCRC16(t *testing.T) {
	for _, v := range crcVectors {
		c := CRC16(v.APDU)
		if byte(c&0xFF) != v.lo || byte(c>>8) != v.hi {
			t.Errorf("%s: crc = %02x %02x, want %02x %02x", v.name, c&0xFF, c>>8, v.lo, v.hi)
		}
	}
}

func TestEncodeFrameStuffing(t *testing.T) {
	apdu := []byte{0x06, 0xd1, 0x05, 0x10, 0x06, 0x10, 0x10, 0x03}
	want := []byte{DLE, STX, 0x06, 0xd1, 0x05, 0x10, 0x10, 0x06, 0x10, 0x10, 0x10, 0x10, 0x03, DLE, ETX, 0x87, 0x07}
	if got := EncodeFrame(apdu); !bytes.Equal(got, want) {
		t.Errorf("EncodeFrame = % X\nwant          % X", got, want)
	}
}

func feedAll(d *Decoder, p []byte) (events []Event) {
	for _, b := range p {
		if ev := d.Feed(b); ev.Kind != EvNone {
			events = append(events, ev)
		}
	}
	return
}

func TestDecoderRoundtrip(t *testing.T) {
	var d Decoder
	for _, v := range crcVectors {
		evs := feedAll(&d, EncodeFrame(v.APDU))
		if len(evs) != 1 || evs[0].Kind != EvFrame || !bytes.Equal(evs[0].APDU, v.APDU) {
			t.Errorf("%s: events %+v", v.name, evs)
		}
	}
}

func TestDecoderControlAndErrors(t *testing.T) {
	var d Decoder
	evs := feedAll(&d, []byte{ACK, NAK, 0x55})
	if len(evs) != 3 || evs[0].Kind != EvAck || evs[1].Kind != EvNak || evs[2].Kind != EvGarbage {
		t.Fatalf("control bytes: %+v", evs)
	}
	bad := EncodeFrame([]byte{0x80, 0x00, 0x00})
	bad[len(bad)-1] ^= 0xFF
	evs = feedAll(&d, bad)
	if len(evs) != 1 || evs[0].Kind != EvBadCRC {
		t.Fatalf("CRC error not detected: %+v", evs)
	}
	evs = feedAll(&d, []byte{DLE, 0x99})
	if len(evs) != 1 || evs[0].Kind != EvProtocol || evs[0].Byte != 0x99 {
		t.Fatalf("protocol error: %+v", evs)
	}
	if d.Busy() {
		t.Fatal("decoder not idle after error")
	}
}

func TestReadAPDU(t *testing.T) {
	short := []byte{0x06, 0x0f, 0x02, 0xaa, 0xbb}
	long := append([]byte{0x06, 0xd3, 0xff, 0x01, 0x01}, bytes.Repeat([]byte{0x41}, 257)...)
	ack := []byte{0x80, 0x00, 0x00}
	r := bytes.NewReader(append(append(append([]byte{}, short...), long...), ack...))
	for i, want := range [][]byte{short, long, ack} {
		got, err := ReadAPDU(r)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("APDU %d: got % X (len %d), err %v", i, got[:min(len(got), 8)], len(got), err)
		}
	}
	if _, err := ReadAPDU(r); err == nil {
		t.Fatal("expected EOF")
	}
}
