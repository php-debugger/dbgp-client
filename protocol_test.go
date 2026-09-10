package dbgp

import (
	"reflect"
	"testing"
)

func TestParseBreakpointSpec(t *testing.T) {
	tests := []struct {
		spec      string
		wantFile  string
		wantLines []int
	}{
		{"file.php:42", "file.php", []int{42}},
		{"file.php:42,55,60", "file.php", []int{42, 55, 60}},
		{`C:\path\file.php:42`, `C:\path\file.php`, []int{42}},
	}

	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			file, lines, err := ParseBreakpointSpec(tt.spec)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if file != tt.wantFile {
				t.Fatalf("file = %q, want %q", file, tt.wantFile)
			}
			if !reflect.DeepEqual(lines, tt.wantLines) {
				t.Fatalf("lines = %v, want %v", lines, tt.wantLines)
			}
		})
	}
}
