package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ergochat/readline"
)

// scriptedReader returns lines and errors in order, then io.EOF.
type scriptedReader struct {
	results []any // string or error
	prompts []string
}

func (s *scriptedReader) ReadLine(prompt string) (string, error) {
	s.prompts = append(s.prompts, prompt)
	if len(s.results) == 0 {
		return "", io.EOF
	}
	next := s.results[0]
	s.results = s.results[1:]
	if err, ok := next.(error); ok {
		return "", err
	}
	return next.(string), nil
}

func TestPlainReader(t *testing.T) {
	r, _, output := newTestREPL(t)
	p := newPlainReader(r, strings.NewReader("sessions\nbreaks\n"))
	for _, want := range []string{"sessions", "breaks"} {
		if line, err := p.ReadLine("> "); err != nil || line != want {
			t.Errorf("ReadLine = %q, %v; want %q", line, err, want)
		}
	}
	if _, err := p.ReadLine("> "); !errors.Is(err, io.EOF) {
		t.Errorf("ReadLine at end = %v, want io.EOF", err)
	}
	if out := output(); out != "> > > \n" {
		t.Errorf("prompts written = %q", out)
	}
}

func TestRunHandlesInterruptAndEOF(t *testing.T) {
	r, _, output := newTestREPL(t)
	lines := &scriptedReader{results: []any{readline.ErrInterrupt, "breaks", readline.ErrInterrupt}}
	r.run(lines)
	if got := strings.Count(output(), "(type quit to exit)"); got != 2 {
		t.Errorf("interrupt hints = %d, want 2:\n%s", got, output())
	}
	if !strings.Contains(output(), "No breakpoints.") {
		t.Errorf("command after an interrupt did not run:\n%s", output())
	}
	if len(lines.prompts) != 4 || lines.prompts[0] != "(dbgp) " {
		t.Errorf("prompts = %q, want 4 ending at EOF", lines.prompts)
	}

	quit := &scriptedReader{results: []any{"quit", "breaks"}}
	r.run(quit)
	if len(quit.prompts) != 1 {
		t.Errorf("run continued after quit: prompts %q", quit.prompts)
	}
}

// complete returns the lines completion offers for line. The completer
// returns suffixes to append to what has been typed.
func complete(line string) []string {
	candidates, _ := completer().Do([]rune(line), len([]rune(line)))
	var got []string
	for _, c := range candidates {
		got = append(got, line+string(c))
	}
	sort.Strings(got)
	return got
}

func TestCompletion(t *testing.T) {
	tests := []struct {
		line string
		want []string
	}{
		{"bre", []string{"break ", "breaks "}},
		{"st", []string{"stack ", "step ", "stop "}},
		{"break c", []string{"break call "}},
		{"break e", []string{"break exception "}},
		{"qu", []string{"quit "}},
	}
	for _, tt := range tests {
		if got := complete(tt.line); strings.Join(got, "|") != strings.Join(tt.want, "|") {
			t.Errorf("complete(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
	// Aliases are not offered.
	for _, c := range complete("") {
		if _, alias := aliases[strings.TrimSpace(c)]; alias {
			t.Errorf("completion offers alias %q", c)
		}
	}
}

func TestReadlineConfig(t *testing.T) {
	cfg := readlineConfig("/tmp/h")
	if cfg.HistoryFile != "/tmp/h" || cfg.HistoryLimit <= 0 || cfg.AutoComplete == nil {
		t.Errorf("config = %+v", cfg)
	}
	if h := defaultHistoryFile(); h != "" && filepath.Base(h) != ".dbgp_history" {
		t.Errorf("defaultHistoryFile() = %q", h)
	}
}

func TestIsTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) {
		t.Error("a pipe is not a terminal")
	}
}
