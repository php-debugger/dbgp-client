package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Most of what the command-line debugger prints comes from the debugged
// script and the engine: program output, variable values, file names and
// messages, often from a web request anyone could send. A terminal acts on
// control characters in text, e.g. escape sequences that rewrite the
// screen, set the window title or write to the clipboard, so those are
// shown as escapes instead.

// safeWriter writes to w with safeText.
type safeWriter struct {
	w io.Writer
}

func (s safeWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(s.w, safeText(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// safeText returns s with what a terminal would act on rather than show
// written as escapes such as \x1b: control characters other than newline
// and tab, invalid UTF-8, and the characters that reorder text. Line
// endings \r\n become \n.
func safeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f, reordersText(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// reordersText reports whether r is a bidirectional embedding, override or
// isolate, which can make text display in another order than it reads.
func reordersText(r rune) bool {
	return (r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩')
}
