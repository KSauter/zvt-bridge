package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestAskDefaultAndValidation(t *testing.T) {
	var out strings.Builder
	p := newPrompter(strings.NewReader("\nbad\n\ngood\n"), &out, false)
	v, err := p.ask("Name", "def", nil)
	if err != nil || v != "def" {
		t.Fatalf("empty answer: %q, %v", v, err)
	}
	validate := func(s string) error {
		if s != "good" {
			return fmt.Errorf("%q rejected", s)
		}
		return nil
	}
	v, err = p.ask("Name", "", validate)
	if err != nil || v != "good" {
		t.Fatalf("validated answer: %q, %v", v, err)
	}
	s := out.String()
	for _, want := range []string{"Name [def]: ", "Name: ", `"bad" rejected`, "A value is required."} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if _, err := p.ask("Name", "", nil); err == nil {
		t.Error("expected an error at end of input")
	}
}

func TestAskLastLineWithoutNewline(t *testing.T) {
	p := newPrompter(strings.NewReader("value"), &strings.Builder{}, false)
	if v, err := p.ask("Name", "", nil); err != nil || v != "value" {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestAssumeYes(t *testing.T) {
	p := newPrompter(strings.NewReader(""), &strings.Builder{}, true)
	if v, err := p.ask("Name", "def", nil); err != nil || v != "def" {
		t.Fatalf("default: %q, %v", v, err)
	}
	if _, err := p.ask("Name", "", nil); !errors.Is(err, errNoDefault) {
		t.Fatalf("no default: %v", err)
	}
	if _, err := p.ask("Name", "def", func(string) error { return errors.New("nope") }); err == nil {
		t.Fatal("invalid default must fail instead of looping")
	}
	if ok, _ := p.yesNo("Go?", false); ok {
		t.Error("yesNo must return the default")
	}
	if i, _ := p.choose("Pick", []string{"a", "b"}, 1); i != 1 {
		t.Errorf("choose returned %d", i)
	}
}

func TestYesNo(t *testing.T) {
	p := newPrompter(strings.NewReader("\nmaybe\nY\nnein\n"), &strings.Builder{}, false)
	for i, want := range []bool{true, true, false} {
		got, err := p.yesNo("Go?", true)
		if err != nil || got != want {
			t.Errorf("answer %d: got %v, %v, want %v", i, got, err, want)
		}
	}
}

func TestChoose(t *testing.T) {
	p := newPrompter(strings.NewReader("9\nx\n2\n\n"), &strings.Builder{}, false)
	if i, err := p.choose("Pick", []string{"a", "b"}, 0); err != nil || i != 1 {
		t.Fatalf("got %d, %v", i, err)
	}
	if i, err := p.choose("Pick", []string{"a", "b"}, 0); err != nil || i != 0 {
		t.Fatalf("default: got %d, %v", i, err)
	}
}
