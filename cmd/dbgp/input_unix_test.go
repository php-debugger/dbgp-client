//go:build !windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ergochat/readline"
)

// The history file stays readable by the user only: when readline creates
// it, when it was readable by others before, and when readline replaces it
// to trim it.
func TestHistoryFileIsPrivate(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0o022)) // a typical umask, restored after
	long := strings.Repeat("eval $secret\n", 1001)
	for name, existing := range map[string]string{"created": "", "existing": "eval $secret\n", "trimmed": long} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history")
			if existing != "" {
				if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := privateHistory(path); err != nil {
				t.Fatal(err)
			}
			cfg := readlineConfig(path)
			cfg.Stdin, cfg.Stdout, cfg.Stderr = io.NopCloser(strings.NewReader("")), io.Discard, io.Discard
			rl, err := readline.NewFromConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := rl.SaveToHistory("eval $token"); err != nil {
				t.Fatal(err)
			}
			_ = rl.Close()

			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("history file mode %o, want 600", perm)
			}
			data, _ := os.ReadFile(path)
			if lines := strings.Count(string(data), "\n"); name == "trimmed" && lines > cfg.HistoryLimit+1 {
				t.Errorf("history has %d lines: readline did not trim it", lines)
			}
		})
	}
}
