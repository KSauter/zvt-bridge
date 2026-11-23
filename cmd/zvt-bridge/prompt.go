package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// prompter asks questions on a console. With assumeYes set it never waits for
// input and answers every question with its default, which turns the setup
// wizard into an unattended installer.
type prompter struct {
	in        *bufio.Reader
	out       io.Writer
	assumeYes bool
}

var errNoDefault = errors.New("a value is required but -yes was given and no default exists")

func newPrompter(in io.Reader, out io.Writer, assumeYes bool) *prompter {
	return &prompter{in: bufio.NewReader(in), out: out, assumeYes: assumeYes}
}

func (p *prompter) printf(format string, a ...any) { fmt.Fprintf(p.out, format, a...) }

// readLine returns the next input line without its line break. The last line
// of the input may end without one.
func (p *prompter) readLine() (string, error) {
	s, err := p.in.ReadString('\n')
	if err != nil && (s == "" || err != io.EOF) {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// ask prints "label [def]: " and returns the answer, or def when the answer is
// empty. validate may reject the answer; the question is then repeated. An
// empty answer without default repeats the question as well.
func (p *prompter) ask(label, def string, validate func(string) error) (string, error) {
	for {
		if def != "" {
			p.printf("%s [%s]: ", label, def)
		} else {
			p.printf("%s: ", label)
		}
		answer := def
		if p.assumeYes {
			p.printf("%s\n", answer)
		} else {
			line, err := p.readLine()
			if err != nil {
				return "", err
			}
			if line = strings.TrimSpace(line); line != "" {
				answer = line
			}
		}
		if answer == "" {
			if p.assumeYes {
				return "", fmt.Errorf("%s: %w", label, errNoDefault)
			}
			p.printf("A value is required.\n")
			continue
		}
		if validate != nil {
			if err := validate(answer); err != nil {
				if p.assumeYes {
					return "", fmt.Errorf("%s: %w", label, err)
				}
				p.printf("%v\n", err)
				continue
			}
		}
		return answer, nil
	}
}

// yesNo asks a yes/no question; an empty answer selects def.
func (p *prompter) yesNo(label string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		p.printf("%s [%s]: ", label, hint)
		if p.assumeYes {
			if def {
				p.printf("y\n")
			} else {
				p.printf("n\n")
			}
			return def, nil
		}
		line, err := p.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes", "j", "ja":
			return true, nil
		case "n", "no", "nein":
			return false, nil
		}
		p.printf("Please answer y or n.\n")
	}
}

// choose prints numbered options and returns the index of the chosen one.
func (p *prompter) choose(label string, options []string, def int) (int, error) {
	p.printf("%s\n", label)
	for i, o := range options {
		p.printf("  %d) %s\n", i+1, o)
	}
	for {
		p.printf("Choice [%d]: ", def+1)
		if p.assumeYes {
			p.printf("%d\n", def+1)
			return def, nil
		}
		line, err := p.readLine()
		if err != nil {
			return 0, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return def, nil
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		p.printf("Please enter a number between 1 and %d.\n", len(options))
	}
}

// pause waits for Enter; used to keep a console window open that would
// otherwise close together with the program.
func (p *prompter) pause(label string) {
	if p.assumeYes {
		return
	}
	p.printf("%s", label)
	p.readLine()
}
