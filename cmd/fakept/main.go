// fakept simulates a payment terminal speaking ZVT over TCP.
// It answers Registration, Authorization, Status-Enquiry and Abort with the
// usual message sequence and, like a real terminal, waits for the ECR's
// application-level acknowledgement 80 00 00 after each of its own messages.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"log"
	"net"
	"time"

	"github.com/KSauter/zvt-bridge/internal/zvt"
)

var verbose bool

func main() {
	listen := flag.String("listen", ":20007", "address the terminal listens on")
	flag.BoolVar(&verbose, "v", false, "log every byte")
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fakept listening on %s", ln.Addr())
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go serve(c)
	}
}

type session struct {
	c     net.Conn
	ackCh chan struct{}
}

// serve reads APDUs in this goroutine and processes commands in a worker, so
// the ECR's 80 00 00 acknowledgements are seen while a command is in progress.
func serve(c net.Conn) {
	defer c.Close()
	log.Printf("ECR connected: %s", c.RemoteAddr())
	s := &session{c: c, ackCh: make(chan struct{}, 1)}
	cmdCh := make(chan []byte, 16)
	go func() {
		for apdu := range cmdCh {
			s.handle(apdu)
		}
	}()
	defer close(cmdCh)
	r := bufio.NewReader(c)
	for {
		apdu, err := zvt.ReadAPDU(r)
		if err != nil {
			log.Printf("ECR disconnected: %v", err)
			return
		}
		logAPDU("ECR → PT", apdu)
		if apdu[0] == 0x80 || apdu[0] == 0x84 {
			select {
			case s.ackCh <- struct{}{}:
			default:
			}
			continue
		}
		cmdCh <- apdu
	}
}

func (s *session) handle(apdu []byte) {
	cmd := uint16(apdu[0])<<8 | uint16(apdu[1])
	s.send([]byte{0x80, 0x00, 0x00}, false)
	switch cmd {
	case 0x0600: // Registration
		s.send([]byte{0x06, 0x0F, 0x00}, true)
	case 0x0601: // Authorization
		amount := bmp04(apdu)
		s.send([]byte{0x04, 0xFF, 0x01, 0x00}, true) // Intermediate Status "insert card"
		time.Sleep(300 * time.Millisecond)
		s.send(statusInfo(amount), true)
		s.send([]byte{0x06, 0x0F, 0x00}, true)
	case 0x0501: // Status Enquiry
		s.send([]byte{0x04, 0x0F, 0x02, 0x27, 0x00}, true)
		s.send([]byte{0x06, 0x0F, 0x00}, true)
	case 0x0602: // Log-Off
	default:
		s.send([]byte{0x06, 0x0F, 0x00}, true)
	}
}

// send writes an APDU and optionally waits for the ECR's 80 00 00.
func (s *session) send(apdu []byte, waitAck bool) {
	if waitAck {
		select {
		case <-s.ackCh:
		default:
		}
	}
	logAPDU("PT → ECR", apdu)
	if _, err := s.c.Write(apdu); err != nil {
		log.Printf("send: %v", err)
		return
	}
	if !waitAck {
		return
	}
	select {
	case <-s.ackCh:
	case <-time.After(3 * time.Second):
		log.Printf("WARNING: no 80 00 00 from ECR for %s", zvt.APDUName(apdu))
	}
}

// bmp04 extracts the amount (BMP 04, 6 bytes BCD) from an Authorization.
func bmp04(apdu []byte) []byte {
	i := bytes.IndexByte(apdu[3:], 0x04)
	if i < 0 || 3+i+7 > len(apdu) {
		return []byte{0, 0, 0, 0, 0, 0}
	}
	return apdu[3+i+1 : 3+i+7]
}

func statusInfo(amount []byte) []byte {
	now := time.Now()
	bcd := func(v int) byte { return byte(v/10<<4 | v%10) }
	data := []byte{0x27, 0x00}                                                       // result code OK
	data = append(data, 0x04)                                                        // amount
	data = append(data, amount...)                                                   //
	data = append(data, 0x49, 0x09, 0x78)                                            // currency EUR
	data = append(data, 0x0B, 0x00, 0x00, 0x01)                                      // trace number
	data = append(data, 0x0C, bcd(now.Hour()), bcd(now.Minute()), bcd(now.Second())) // time
	data = append(data, 0x0D, bcd(int(now.Month())), bcd(now.Day()))                 // date
	return append([]byte{0x04, 0x0F, byte(len(data))}, data...)
}

func logAPDU(dir string, apdu []byte) {
	max := 16
	if verbose {
		max = 0
	}
	log.Printf("%s  %-20s %3d B  %s", dir, zvt.APDUName(apdu), len(apdu), zvt.HexDump(apdu, max))
}
