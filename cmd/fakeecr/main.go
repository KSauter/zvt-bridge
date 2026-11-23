// fakeecr simulates a cash register speaking ZVT over a serial port.
// It performs Registration, Authorization and Status-Enquiry (selectable with
// -steps) and exits with code 0 when every sequence ended with a Completion.
// Against a real terminal, leave the Authorization out unless you want the
// terminal to ask for a card.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"go.bug.st/serial"

	"github.com/KSauter/zvt-bridge/internal/zvt"
)

var verbose bool

type ecr struct {
	port    serial.Port
	events  chan zvt.Event
	pending [][]byte
	timeout time.Duration
}

func main() {
	portName := flag.String("serial", "", "serial port (required)")
	baud := flag.Int("baud", 9600, "baud rate")
	amount := flag.Int("amount", 100, "amount in cents for the Authorization")
	password := flag.String("password", "123456", "six-digit terminal password for the Registration")
	stepList := flag.String("steps", "registration,authorization,status", "comma-separated sequences to run")
	timeout := flag.Duration("timeout", 10*time.Second, "time to wait for terminal messages")
	flag.BoolVar(&verbose, "v", false, "log every byte")
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if *portName == "" {
		flag.Usage()
		os.Exit(2)
	}
	port, err := serial.Open(*portName, &serial.Mode{BaudRate: *baud, DataBits: 8, Parity: serial.NoParity, StopBits: serial.TwoStopBits})
	if err != nil {
		log.Fatalf("serial port %s: %v", *portName, err)
	}
	defer port.Close()

	e := &ecr{port: port, events: make(chan zvt.Event, 64), timeout: *timeout}
	go e.read()

	pw, err := bcdPassword(*password)
	if err != nil {
		log.Fatal(err)
	}
	all := map[string]struct {
		name string
		apdu []byte
	}{
		"registration":  {"Registration", append(append([]byte{0x06, 0x00, 0x06}, pw...), 0x78, 0x09, 0x78)},
		"authorization": {"Authorization", authorization(*amount)},
		"status":        {"Status-Enquiry", []byte{0x05, 0x01, 0x00}},
	}
	var steps []struct {
		name string
		apdu []byte
	}
	for _, key := range strings.Split(*stepList, ",") {
		st, ok := all[strings.ToLower(strings.TrimSpace(key))]
		if !ok {
			log.Fatalf("unknown step %q (use registration, authorization, status)", key)
		}
		steps = append(steps, st)
	}
	failed := 0
	for _, st := range steps {
		log.Printf("=== %s ===", st.name)
		if err := e.transaction(st.apdu); err != nil {
			log.Printf("FAILED %s: %v", st.name, err)
			failed++
		} else {
			log.Printf("OK %s", st.name)
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
	log.Println("all sequences completed successfully")
}

// bcdPassword packs a six-digit password into three BCD bytes.
func bcdPassword(s string) ([]byte, error) {
	if len(s) != 6 {
		return nil, fmt.Errorf("password %q must have six digits", s)
	}
	out := make([]byte, 3)
	for i := 0; i < 6; i++ {
		if s[i] < '0' || s[i] > '9' {
			return nil, fmt.Errorf("password %q must have six digits", s)
		}
		out[i/2] = out[i/2]<<4 | (s[i] - '0')
	}
	return out, nil
}

func authorization(cents int) []byte {
	amt := make([]byte, 6)
	for i := 5; i >= 0; i-- {
		amt[i] = byte(cents%10 | (cents/10%10)<<4)
		cents /= 100
	}
	data := append([]byte{0x04}, amt...)
	data = append(data, 0x49, 0x09, 0x78) // currency EUR
	data = append(data, 0x19, 0x40)       // payment type: terminal decides
	return append([]byte{0x06, 0x01, byte(len(data))}, data...)
}

func (e *ecr) read() {
	var d zvt.Decoder
	buf := make([]byte, 256)
	for {
		n, err := e.port.Read(buf)
		for i := 0; i < n; i++ {
			if verbose {
				log.Printf("  ← raw %02X", buf[i])
			}
			if ev := d.Feed(buf[i]); ev.Kind != zvt.EvNone {
				e.events <- ev
			}
		}
		if err != nil {
			e.events <- zvt.Event{Kind: zvt.EvGarbage}
			return
		}
	}
}

// send frames an APDU, writes it and waits for the transport ACK.
// Frames arriving meanwhile are acknowledged and queued.
func (e *ecr) send(apdu []byte) error {
	frame := zvt.EncodeFrame(apdu)
	for attempt := 1; attempt <= 3; attempt++ {
		logAPDU("ECR → bridge", apdu)
		if _, err := e.port.Write(frame); err != nil {
			return err
		}
		timer := time.After(2 * time.Second)
	wait:
		for {
			select {
			case ev := <-e.events:
				switch ev.Kind {
				case zvt.EvAck:
					return nil
				case zvt.EvNak:
					log.Printf("NAK received, attempt %d", attempt)
					break wait
				case zvt.EvFrame:
					e.ackFrame(ev.APDU)
					e.pending = append(e.pending, ev.APDU)
				case zvt.EvBadCRC:
					e.port.Write([]byte{zvt.NAK})
				}
			case <-timer:
				log.Printf("no ACK, attempt %d", attempt)
				break wait
			}
		}
	}
	return errors.New("no ACK from bridge")
}

func (e *ecr) ackFrame(apdu []byte) {
	e.port.Write([]byte{zvt.ACK})
	logAPDU("bridge → ECR", apdu)
}

// next returns the next received APDU, acknowledging it on transport level.
func (e *ecr) next() ([]byte, error) {
	if len(e.pending) > 0 {
		a := e.pending[0]
		e.pending = e.pending[1:]
		return a, nil
	}
	timer := time.After(e.timeout)
	for {
		select {
		case ev := <-e.events:
			switch ev.Kind {
			case zvt.EvFrame:
				e.ackFrame(ev.APDU)
				return ev.APDU, nil
			case zvt.EvBadCRC:
				e.port.Write([]byte{zvt.NAK})
			case zvt.EvAck, zvt.EvNak:
				log.Printf("unexpected ACK/NAK ignored")
			}
		case <-timer:
			return nil, fmt.Errorf("no message from terminal within %s", e.timeout)
		}
	}
}

// transaction sends a command and processes the responses up to the Completion.
func (e *ecr) transaction(cmd []byte) error {
	if err := e.send(cmd); err != nil {
		return err
	}
	accepted := false
	for {
		apdu, err := e.next()
		if err != nil {
			return err
		}
		switch {
		case apdu[0] == 0x80:
			accepted = true
		case apdu[0] == 0x84:
			return fmt.Errorf("terminal rejected: %s", zvt.APDUName(apdu))
		case apdu[0] == 0x06 && apdu[1] == 0x0F: // Completion
			if !accepted {
				log.Printf("note: Completion without preceding 80 00 00")
			}
			return e.send([]byte{0x80, 0x00, 0x00})
		default: // Intermediate Status, Status Information, Print Line, …
			if apdu[0] == 0x04 && apdu[1] == 0x0F && len(apdu) > 4 && apdu[3] == 0x27 {
				log.Printf("    result code %02X", apdu[4])
			}
			if err := e.send([]byte{0x80, 0x00, 0x00}); err != nil {
				return err
			}
		}
	}
}

func logAPDU(dir string, apdu []byte) {
	max := 16
	if verbose {
		max = 0
	}
	log.Printf("%s  %-20s %3d B  %s", dir, zvt.APDUName(apdu), len(apdu), zvt.HexDump(apdu, max))
}
