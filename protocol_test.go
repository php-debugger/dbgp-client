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
		{"file:///tmp/test.php:42", "file:///tmp/test.php", []int{42}},
		{"file://C:/path/test.php:42", "file://C:/path/test.php", []int{42}},
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

func TestFileURIConversions(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		expectURI string
	}{
		{
			name:      "unix path with spaces",
			path:      "/tmp/test dir/file.php",
			expectURI: "file:///tmp/test%20dir/file.php",
		},
		{
			name:      "windows drive path",
			path:      `C:\path with spaces\file.php`,
			expectURI: "file:///C:/path%20with%20spaces/file.php",
		},
		{
			name:      "windows unc path",
			path:      `\\server\share\dir\file.php`,
			expectURI: "file://server/share/dir/file.php",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURI := MakeFileURI(tt.path)
			if gotURI != tt.expectURI {
				t.Fatalf("MakeFileURI() = %q, want %q", gotURI, tt.expectURI)
			}

			gotPath := FormatFileURI(gotURI)
			if gotPath == "" {
				t.Fatalf("FormatFileURI() returned empty path")
			}
		})
	}
}
