package dbgp

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestParseInitFixture(t *testing.T) {
	init, err := ParseInit([]byte(fixture(t, "init")))
	if err != nil {
		t.Fatal(err)
	}
	want := InitPacket{
		AppID: fixtureAppID, Language: "PHP", Protocol: "1.0", FileURI: fixtureScriptURI,
		Engine: Engine{Name: "PHP Debugger", Version: "0.3.3"}, EngineVersion: "0.3.3",
	}
	init.XMLName = want.XMLName
	if *init != want {
		t.Errorf("init = %+v, want %+v", *init, want)
	}
}

func TestParseInitRejectsResponse(t *testing.T) {
	if _, err := ParseInit([]byte(fixture(t, "status_break"))); err == nil {
		t.Error("ParseInit accepted a response packet")
	}
}

// Every recorded Xdebug response must parse, with its command attribute.
func TestParseResponseFixtures(t *testing.T) {
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".xml")
		data := []byte(fixture(t, name))
		if !bytes.Contains(data, []byte("<response")) {
			continue
		}
		t.Run(name, func(t *testing.T) {
			resp, err := ParseResponse(data)
			if err != nil {
				t.Fatal(err)
			}
			if want := attr(data, "command"); resp.Command != want {
				t.Errorf("Command = %q, want %q", resp.Command, want)
			}
			if want := attr(data, "status"); resp.Status != want {
				t.Errorf("Status = %q, want %q", resp.Status, want)
			}
			if wantErr := bytes.Contains(data, []byte("<error")); (resp.Error != nil) != wantErr {
				t.Errorf("Error = %+v, want error: %v", resp.Error, wantErr)
			}
		})
	}
}

func TestParseResponseRejectsOtherPackets(t *testing.T) {
	for _, name := range []string{"init", "stream_stdout", "notify_breakpoint_resolved"} {
		if _, err := ParseResponse([]byte(fixture(t, name))); err == nil {
			t.Errorf("ParseResponse accepted %s", name)
		}
	}
}

func TestParseResponseFields(t *testing.T) {
	parse := func(name string) *Response {
		t.Helper()
		resp, err := ParseResponse([]byte(fixture(t, name)))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if r := parse("error_breakpoint_not_found"); r.Error.Code != 205 || r.Error.Message != "no such breakpoint" {
		t.Errorf("error = %+v", r.Error)
	}
	if r := parse("breakpoint_set_line"); r.BreakpointID != 42420001 {
		t.Errorf("BreakpointID = %d", r.BreakpointID)
	}
	if r := parse("context_get_locals"); len(r.Properties) != 3 || r.Properties[0].FullName != "$a" {
		t.Errorf("Properties = %+v", r.Properties)
	}
	if r := parse("stack_get"); len(r.Stack) != 2 || r.Stack[1].Where != "{main}" {
		t.Errorf("Stack = %+v", r.Stack)
	}
	if r := parse("source"); r.Encoding != "base64" || r.Value == "" {
		t.Errorf("source Encoding = %q, Value = %q", r.Encoding, r.Value)
	}
	for name, want := range map[string]int{"run_break": lineAddBody, "step_over": lineAddReturn, "run_break_conditional": lineLoopBody} {
		file, line := parse(name).ParseMessage()
		if file != "/app/basic.php" || line != want {
			t.Errorf("%s: ParseMessage = %s:%d, want /app/basic.php:%d", name, file, line, want)
		}
	}
	if file, line := parse("stop").ParseMessage(); file != "" || line != 0 {
		t.Errorf("stop: ParseMessage = %q:%d, want empty", file, line)
	}
}

func TestCharsetReader(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		value    []byte
		want     string
	}{
		{"utf-8", "UTF-8", []byte("h\xc3\xa9"), "hé"},
		{"latin1", "iso-8859-1", []byte("h\xe9"), "hé"},
		{"windows-1252 override", "windows-1252", []byte("\x80\x93x\x94"), "€“x”"},
		{"windows-1252 latin range", "windows-1252", []byte("\xe9"), "é"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := append([]byte(`<?xml version="1.0" encoding="`+tt.encoding+`"?><response command="eval" transaction_id="1"><property type="string">`), tt.value...)
			data = append(data, []byte(`</property></response>`)...)
			resp, err := ParseResponse(data)
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Properties[0].Value; got != tt.want {
				t.Errorf("value = %q, want %q", got, tt.want)
			}
		})
	}

	if _, err := ParseResponse([]byte(`<?xml version="1.0" encoding="koi8-r"?><response/>`)); err == nil {
		t.Error("unsupported charset accepted")
	}
}

func TestParseBreakpointSpecErrors(t *testing.T) {
	for _, spec := range []string{"", "file.php", ":42", "file.php:", "file.php:abc", "file.php:4a", "file.php:42,,43", "file.php:-1"} {
		if file, lines, err := ParseBreakpointSpec(spec); err == nil {
			t.Errorf("ParseBreakpointSpec(%q) = %q, %v; want error", spec, file, lines)
		}
	}
}

func TestFileURIRoundTrip(t *testing.T) {
	for _, path := range []string{"/tmp/a.php", "/tmp/test dir/file.php", "/tmp/ümlaut/#hash%.php"} {
		uri := MakeFileURI(path)
		if !strings.HasPrefix(uri, "file:///") {
			t.Errorf("MakeFileURI(%q) = %q", path, uri)
		}
		if got := FormatFileURI(uri); got != filepath.FromSlash(path) {
			t.Errorf("FormatFileURI(MakeFileURI(%q)) = %q (uri %q)", path, got, uri)
		}
	}
	if got := FormatFileURI("file:///C:/path%20with%20spaces/file.php"); got != filepath.FromSlash("C:/path with spaces/file.php") {
		t.Errorf("windows drive URI -> %q", got)
	}
	if got := FormatFileURI("file://server/share/file.php"); got != "//server"+filepath.FromSlash("/share/file.php") {
		t.Errorf("UNC URI -> %q", got)
	}
	if got := MakeFileURI("file:///already/a%20uri.php"); got != "file:///already/a%20uri.php" {
		t.Errorf("MakeFileURI(uri) = %q", got)
	}
	if got := MakeFileURI("relative/file.php"); got != "file:///relative/file.php" {
		t.Errorf("MakeFileURI(relative) = %q", got)
	}
	if got := FormatFileURI("dbgp://eval/1"); got != "dbgp://eval/1" {
		t.Errorf("FormatFileURI(non-file) = %q", got)
	}
}

func TestFormatStack(t *testing.T) {
	frames := []StackFrame{
		{Level: 0, Where: "add", Filename: "file:///app/basic.php", Lineno: 24},
		{Level: 1, Where: "{main}", Filename: "file:///app/basic.php", Lineno: 36},
	}
	want := []string{"#0 add() at /app/basic.php:24", "#1 {main}() at /app/basic.php:36"}
	if got := FormatStack(frames); !reflect.DeepEqual(got, want) {
		t.Errorf("FormatStack = %q, want %q", got, want)
	}
}

func TestFormatStackDecodesURI(t *testing.T) {
	got := FormatStack([]StackFrame{{Where: "f", Filename: "file:///my%20app/a.php", Lineno: 1}})
	if want := "#0 f() at /my app/a.php:1"; got[0] != want {
		t.Errorf("FormatStack = %q, want %q", got[0], want)
	}
}

func TestParseBreakpoints(t *testing.T) {
	resp, err := ParseResponse([]byte(fixture(t, "breakpoint_list")))
	if err != nil {
		t.Fatal(err)
	}
	want := []BreakpointInfo{
		{ID: 42420001, Type: "line", Filename: fixtureScriptURI, Lineno: 24, State: "enabled", Resolved: "resolved"},
		{ID: 42420002, Type: "conditional", Filename: fixtureScriptURI, Lineno: 39, State: "enabled", Resolved: "resolved", Expression: "$i == 3"},
		{ID: 42420003, Type: "call", Function: "add", State: "disabled", Resolved: "resolved"},
		{ID: 42420004, Type: "exception", Exception: "Exception", State: "enabled", Resolved: "resolved"},
	}
	if !reflect.DeepEqual(resp.Breakpoints, want) {
		t.Errorf("Breakpoints =\n%+v\nwant\n%+v", resp.Breakpoints, want)
	}

	resp, err = ParseResponse([]byte(fixture(t, "breakpoint_get")))
	if err != nil || len(resp.Breakpoints) != 1 || resp.Breakpoints[0].Expression != "$i == 3" {
		t.Errorf("breakpoint_get = %+v, %v", resp, err)
	}
}

func TestParseBreakpointExpressionForms(t *testing.T) {
	parse := func(bp string) (*Response, error) {
		return ParseResponse([]byte(`<response command="breakpoint_get" transaction_id="1">` + bp + `</response>`))
	}
	resp, err := parse(`<breakpoint id="1" type="conditional" expression="$a &gt; 1"></breakpoint>`)
	if err != nil || resp.Breakpoints[0].Expression != "$a > 1" {
		t.Errorf("attribute form: %+v, %v", resp, err)
	}
	resp, err = parse(`<breakpoint id="1" type="conditional"><expression>$b</expression></breakpoint>`)
	if err != nil || resp.Breakpoints[0].Expression != "$b" {
		t.Errorf("plain element form: %+v, %v", resp, err)
	}
	if _, err := parse(`<breakpoint id="1"><expression encoding="base64">!!</expression></breakpoint>`); err == nil {
		t.Error("invalid base64 expression accepted")
	}
	if _, err := parse(`<breakpoint id="1"><expression encoding="rot13">x</expression></breakpoint>`); err == nil {
		t.Error("unknown expression encoding accepted")
	}
}

func TestParseFeatureGetSupported(t *testing.T) {
	for name, want := range map[string]string{"feature_get": "1", "feature_get_unsupported": "0"} {
		resp, err := ParseResponse([]byte(fixture(t, name)))
		if err != nil || resp.Supported != want {
			t.Errorf("%s: Supported = %q, %v; want %q", name, resp.Supported, err, want)
		}
	}
}
