package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeText(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                 "plain text",
		"tabs\tand\nnewlines\n":      "tabs\tand\nnewlines\n",
		"windows\r\nlines\r\n":       "windows\nlines\n",
		"a lone\rreturn":             `a lone\x0dreturn`,
		"\x1b[2Jcleared":             `\x1b[2Jcleared`,
		"\x1b]52;c;cGF5bG9hZA==\x07": `\x1b]52;c;cGF5bG9hZA==\x07`,
		"bell\a and nul\x00":         `bell\x07 and nul\x00`,
		"del\x7f":                    `del\x7f`,
		"c1 \u009b2J":                `c1 \u009b2J`,
		"invalid \xff\xfe utf-8":     `invalid \xff\xfe utf-8`,
		"evil\u202egnp.php":          `evil\u202egnp.php`,
		"isolate \u2066x\u2069":      `isolate \u2066x\u2069`,
		"café 日本語 🐘 \ufffd":          "café 日本語 🐘 \ufffd",
		"$user[\"name\"] = 'Ana'":    "$user[\"name\"] = 'Ana'",
		"/var/www/html/index.php:12": "/var/www/html/index.php:12",
	} {
		if got := safeText(in); got != want {
			t.Errorf("safeText(%q) = %q, want %q", in, got, want)
		}
	}
}

// Everything the REPL prints goes through safeText, including the prompt.
func TestREPLPrintsSafely(t *testing.T) {
	var out bytes.Buffer
	r := newREPL(nil, &out)
	r.printf("%s\n", "\x1b]0;title\x07")
	if got := out.String(); got != `\x1b]0;title\x07`+"\n" {
		t.Errorf("printed %q", got)
	}
}

// Escape sequences from a script, in what it prints and in its variables,
// reach the terminal as text, not as escapes it acts on.
func TestREPLShowsScriptEscapesAsText(t *testing.T) {
	requireEngine(t)
	script := filepath.Join(t.TempDir(), "escapes.php")
	code := "<?php\n$evil = \"\\e]52;c;cGF5bG9hZA==\\x07\\e[2J\";\necho $evil, \"done\\n\";\n$x = 1;\n"
	if err := os.WriteFile(script, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	r, srv, output := newTestREPL(t)
	go r.announceSessions()
	php := exec.Command("php", "-dxdebug.mode=debug", "-dxdebug.start_with_request=yes",
		fmt.Sprintf("-dxdebug.client_port=%d", srv.Port()), "-ddisplay_errors=0", script)
	if err := php.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = php.Process.Kill(); _ = php.Wait() })

	commands := []string{"break " + script + ":4", "run", "vars", "print $evil", "value $evil", "output", "stop"}
	r.run(newPlainReader(r, strings.NewReader(strings.Join(commands, "\n"))))

	out := output()
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("escape characters reached the terminal:\n%q", out)
	}
	escaped := `\x1b]52;c;cGF5bG9hZA==\x07\x1b[2J`
	for _, want := range []string{
		"$evil                          string   = \"" + escaped + "\"", // vars
		escaped + "done\n", // output
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, escaped); n < 4 {
		t.Errorf("the value is shown escaped %d times, want 4 (vars, print, value, output):\n%s", n, out)
	}
}
