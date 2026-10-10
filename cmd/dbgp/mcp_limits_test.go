package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stoppedInLarge runs testdata/php/large.php to its deepest call, after it
// has printed its text and raised its warnings.
func stoppedInLarge(t *testing.T) *mcp.ClientSession {
	t.Helper()
	cs, _, _ := stoppedAt(t, "large.php", 8)
	return cs
}

func TestMCPLimitText(t *testing.T) {
	cs := stoppedInLarge(t)
	text := strings.Repeat("é", 50000)

	// Each part ends on a character boundary and the parts add up.
	var value strings.Builder
	var part variableValueOutput
	for offset := 0; ; offset = part.Next {
		callTool(t, cs, "variable_value", map[string]any{"name": "$text", "depth": 61, "offset": offset}, &part)
		if len(part.Value) > maxTextBytes || !utf8.ValidString(part.Value) || part.Size != len(text) {
			t.Fatalf("part at %d: %d bytes, size %d", offset, len(part.Value), part.Size)
		}
		value.WriteString(part.Value)
		if part.Next == 0 {
			break
		}
	}
	if value.String() != text {
		t.Errorf("variable_value parts make %d bytes, want %d", value.Len(), len(text))
	}
	if msg := callToolError(t, cs, "variable_value", map[string]any{"name": "$text", "depth": 61, "offset": len(text) + 1}); !strings.Contains(msg, "offset must be from 0 to 100000") {
		t.Errorf("offset past the end: %q", msg)
	}

	var output strings.Builder
	var printed outputOutput
	calls := 0
	for {
		callTool(t, cs, "output", nil, &printed)
		calls++
		if len(printed.Output) > maxTextBytes || !utf8.ValidString(printed.Output) {
			t.Fatalf("output part %d: %d bytes", calls, len(printed.Output))
		}
		output.WriteString(printed.Output)
		if !printed.More {
			break
		}
	}
	if output.String() != text || calls != 4 {
		t.Errorf("output parts make %d bytes in %d calls, want %d in 4", output.Len(), calls, len(text))
	}
}

func TestMCPLimitLists(t *testing.T) {
	cs := stoppedInLarge(t)

	// 61 calls of down() and the script itself.
	var stack stackOutput
	callTool(t, cs, "stack", nil, &stack)
	if len(stack.Frames) != maxListItems || stack.Pages != 2 || stack.Frames[0].Depth != 0 {
		t.Errorf("stack page 0: %d frames of %d pages", len(stack.Frames), stack.Pages)
	}
	callTool(t, cs, "stack", map[string]any{"page": 1}, &stack)
	if len(stack.Frames) != 12 || stack.Frames[0].Depth != 50 || stack.Frames[11].Function != "{main}" {
		t.Errorf("stack page 1: %+v", stack.Frames)
	}
	if msg := callToolError(t, cs, "stack", map[string]any{"page": 2}); !strings.Contains(msg, "page must be from 0 to 1") {
		t.Errorf("stack page 2: %q", msg)
	}

	// $v1 to $v60, $text, $i and $line.
	var vars variablesOutput
	callTool(t, cs, "variables", map[string]any{"depth": 61}, &vars)
	all := vars.Variables
	if len(all) != maxListItems || vars.Pages != 2 {
		t.Errorf("variables page 0: %d of %d pages", len(all), vars.Pages)
	}
	callTool(t, cs, "variables", map[string]any{"depth": 61, "page": 1}, &vars)
	all = append(all, vars.Variables...)
	if len(all) != 63 || vars.Page != 1 {
		t.Errorf("variables: %d in all, want 63", len(all))
	}

	// 60 different warnings, then one raised 10 times.
	var raised warningsOutput
	callTool(t, cs, "warnings", nil, &raised)
	if len(raised.Warnings) != maxListItems || !raised.More || raised.Warnings[49].Message != "warning 50" {
		t.Fatalf("warnings: %d, more %v", len(raised.Warnings), raised.More)
	}
	callTool(t, cs, "warnings", nil, &raised)
	last := raised.Warnings[len(raised.Warnings)-1]
	if len(raised.Warnings) != 11 || raised.More || raised.Warnings[0].Message != "warning 51" ||
		last.Message != "again" || last.Count != 10 || last.Line != 20 {
		t.Errorf("warnings after the first 50: %+v", raised.Warnings)
	}
}

func TestMCPLimitSourceLine(t *testing.T) {
	cs := stoppedInLarge(t)
	var out sourceOutput
	callTool(t, cs, "source", map[string]any{"line": 22, "around": 1}, &out)
	if long := out.Lines[1]; long.Line != 22 || len(long.Text) != maxLineBytes || !long.Truncated {
		t.Errorf("line 22: %d bytes, truncated %v", len(long.Text), long.Truncated)
	}
	if short := out.Lines[0]; short.Truncated {
		t.Errorf("line 21 truncated: %q", short.Text)
	}
}
