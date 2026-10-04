package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/ergochat/readline"
)

// lineReader reads command lines. ReadLine returns io.EOF at the end of
// input, and readline.ErrInterrupt when Ctrl-C is pressed at the prompt.
type lineReader interface {
	ReadLine(prompt string) (string, error)
}

// plainReader reads lines from a pipe or file, printing prompts itself.
type plainReader struct {
	scanner *bufio.Scanner
	print   func(string)
}

func newPlainReader(r *repl, in io.Reader) *plainReader {
	return &plainReader{scanner: bufio.NewScanner(in), print: func(s string) { r.printf("%s", s) }}
}

func (p *plainReader) ReadLine(prompt string) (string, error) {
	p.print(prompt)
	if !p.scanner.Scan() {
		p.print("\n")
		if err := p.scanner.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return p.scanner.Text(), nil
}

// terminalReader reads lines with editing, history and tab completion.
type terminalReader struct {
	rl *readline.Instance
}

func (t *terminalReader) ReadLine(prompt string) (string, error) {
	t.rl.SetPrompt(prompt)
	return t.rl.ReadLine()
}

// newTerminalReader starts line editing on the terminal. Output written to
// the returned reader's Stdout redraws the prompt, so messages that arrive
// while a line is being typed do not break it. historyFile may be empty.
func newTerminalReader(historyFile string) (*terminalReader, error) {
	rl, err := readline.NewFromConfig(readlineConfig(historyFile))
	if err != nil {
		return nil, err
	}
	return &terminalReader{rl: rl}, nil
}

func readlineConfig(historyFile string) *readline.Config {
	return &readline.Config{
		Prompt:          "(dbgp) ",
		HistoryFile:     historyFile,
		HistoryLimit:    1000,
		AutoComplete:    completer(),
		InterruptPrompt: "^C",
		EOFPrompt:       "quit",
	}
}

// completer completes command names and breakpoint kinds.
func completer() *readline.PrefixCompleter {
	names := make([]string, 0, len(commands)+1)
	for name := range commands {
		if _, alias := aliases[name]; !alias {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	items := make([]*readline.PrefixCompleter, 0, len(names))
	for _, name := range names {
		if name == "break" {
			items = append(items, readline.PcItem(name,
				readline.PcItem("call"), readline.PcItem("return"), readline.PcItem("exception")))
			continue
		}
		items = append(items, readline.PcItem(name))
	}
	return readline.NewPrefixCompleter(items...)
}

// defaultHistoryFile is ~/.dbgp_history, or "" if there is no home.
func defaultHistoryFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".dbgp_history")
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// isInterrupt reports whether err is Ctrl-C at the prompt.
func isInterrupt(err error) bool {
	return errors.Is(err, readline.ErrInterrupt)
}
