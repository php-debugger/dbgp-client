package dbgp

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormatSimpleValue(t *testing.T) {
	tests := []struct {
		typ       string
		content   string
		classname string
		want      string
	}{
		{"string", "hello", "", `"hello"`},
		{"string", "", "", `""`},
		{"int", "42", "", "42"},
		{"bool", "1", "", "true"},
		{"bool", "0", "", "false"},
		{"null", "", "", "null"},
		{"array", "", "App\\Foo", "array[]"},
		{"array", "", "", "array[]"},
		{"object", "", "App\\Service", "App\\Service{}"},
	}

	for _, tt := range tests {
		got := formatSimpleValue(tt.typ, tt.content, tt.classname)
		if got != tt.want {
			t.Errorf("formatSimpleValue(%q, %q, %q) = %q, want %q", tt.typ, tt.content, tt.classname, got, tt.want)
		}
	}
}

func TestFormatPropertyValue(t *testing.T) {
	tests := []struct {
		name string
		prop Property
		want string
	}{
		{
			name: "base64 string",
			prop: Property{Type: "string", Encoding: "base64", Value: "SGVsbG8sIFdvcmxkIQ=="},
			want: `"Hello, World!"`,
		},
		{
			name: "int",
			prop: Property{Type: "int", Value: "42"},
			want: "42",
		},
		{
			name: "bool true",
			prop: Property{Type: "bool", Value: "1"},
			want: "true",
		},
		{
			name: "array with children",
			prop: Property{Type: "array", Children: 5},
			want: "array[5]",
		},
		{
			name: "object with classname",
			prop: Property{Type: "object", ClassName: "App\\Service", Children: 2},
			want: "App\\Service",
		},
		{
			name: "string starting with angle bracket",
			prop: Property{Type: "string", Value: "<html>"},
			want: `"<html>"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatPropertyValue(tt.prop)
			if got != tt.want {
				t.Errorf("formatPropertyValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseVariablesFromRealXML(t *testing.T) {
	data, err := os.ReadFile("testdata/context_get_complex.xml")
	if err != nil {
		t.Fatalf("read test file: %v", err)
	}

	vars := ParseVariables(string(data))

	findVar := func(name string) *Variable {
		for i := range vars {
			if vars[i].Name == name {
				return &vars[i]
			}
		}
		return nil
	}

	tests := []struct {
		name      string
		wantType  string
		wantValue string
		wantLevel int
	}{
		{"$count", "string", `"Hello, World!"`, 0},
		{"$name", "string", `"World"`, 0},
		{"$sum", "int", "15", 0},
		{"$numbers", "array", "array[5]", 0},
		{"$numbers[0]", "int", "1", 1},
		{"$user", "array", "array[3]", 0},
		{"$obj", "object", `App\Service\MyService`, 0},
		{"$obj->id", "int", "42", 1},
		{"$emptyArray", "array", "array[]", 0},
		{"$nullVal", "null", "null", 0},
		{"$boolVal", "bool", "true", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := findVar(tt.name)
			if v == nil {
				t.Fatalf("variable %s not found", tt.name)
			}
			if v.Type != tt.wantType {
				t.Errorf("type = %q, want %q", v.Type, tt.wantType)
			}
			if v.Value != tt.wantValue {
				t.Errorf("value = %q, want %q", v.Value, tt.wantValue)
			}
			if v.Level != tt.wantLevel {
				t.Errorf("level = %d, want %d", v.Level, tt.wantLevel)
			}
		})
	}
}

func TestParseAllVariablesFromRealXML(t *testing.T) {
	data, err := os.ReadFile("testdata/context_get_complex.xml")
	if err != nil {
		t.Fatalf("read test file: %v", err)
	}

	vars := ParseAllVariables(string(data))

	findVar := func(name string) *Variable {
		for i := range vars {
			if vars[i].Name == name {
				return &vars[i]
			}
		}
		return nil
	}

	tests := []struct {
		name      string
		wantType  string
		wantValue string
		wantLevel int
	}{
		{"$obj", "object", `App\Service\MyService`, 0},
		{"$obj->cache", "object", `App\Cache`, 1},
		{"$obj->cache->items", "array", "array[3]", 2},
		{"$obj->cache->items[0]", "string", `"item1"`, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := findVar(tt.name)
			if v == nil {
				t.Fatalf("variable %s not found", tt.name)
			}
			if v.Type != tt.wantType {
				t.Errorf("type = %q, want %q", v.Type, tt.wantType)
			}
			if v.Value != tt.wantValue {
				t.Errorf("value = %q, want %q", v.Value, tt.wantValue)
			}
			if v.Level != tt.wantLevel {
				t.Errorf("level = %d, want %d", v.Level, tt.wantLevel)
			}
		})
	}
}

// Variables from a recorded context_get (max_children=10).
func TestParseVariablesFromXdebugFixture(t *testing.T) {
	vars := ParseVariables(fixture(t, "context_get_main"))
	got := map[string]Variable{}
	for _, v := range vars {
		got[v.Name] = v
	}
	want := []Variable{
		{Name: "$float", Type: "float", Value: "1.5"},
		{Name: "$greeting", Type: "string", Value: `"Héllo, wörld"`},
		{Name: "$i", Type: "uninitialized", Value: "null"},
		{Name: "$long", Type: "string", Value: `"` + strings.Repeat("abcdefghij", 6)[:55] + `..."`},
		{Name: "$nullVal", Type: "null", Value: "null"},
		{Name: "$numbers", Type: "array", Value: "array[40]"},
		{Name: "$numbers[9]", Type: "int", Value: "10", Level: 1},
		{Name: "$obj", Type: "object", Value: "Service"},
		{Name: "$obj->id", Type: "int", Value: "42", Level: 1},
		{Name: "$obj->cache", Type: "object", Value: "Cache", Level: 1},
		{Name: "$obj->name", Type: "string", Value: `"svc"`, Level: 1},
		{Name: `$user["active"]`, Type: "bool", Value: "true", Level: 1},
	}
	for _, w := range want {
		if g, ok := got[w.Name]; !ok || g != w {
			t.Errorf("%s = %+v, want %+v", w.Name, g, w)
		}
	}
	// Only the first page of children (pagesize 10) is present.
	if _, ok := got["$numbers[10]"]; ok {
		t.Error("$numbers[10] present beyond the first page")
	}
}

func TestParseVariablesFromPropertyGet(t *testing.T) {
	vars := ParseVariables(fixture(t, "property_get_page"))
	if len(vars) != 11 || vars[0].Value != "array[40]" || vars[1].Name != "$numbers[10]" || vars[1].Value != "11" {
		t.Errorf("vars = %+v", vars)
	}
}

func TestParseVariablesMalformed(t *testing.T) {
	for _, xml := range []string{"", "<response", "<init/>", "not xml"} {
		if vars := ParseVariables(xml); vars != nil {
			t.Errorf("ParseVariables(%q) = %+v, want nil", xml, vars)
		}
		if vars := ParseAllVariables(xml); vars != nil {
			t.Errorf("ParseAllVariables(%q) = %+v, want nil", xml, vars)
		}
	}
}

func TestFormatSimpleValueEdgeCases(t *testing.T) {
	tests := []struct{ typ, content, want string }{
		{"bool", "true", "true"},
		{"bool", "", "false"},
		{"uninitialized", "", "null"},
		{"float", "1.5", "1.5"},
		{"object", "", "object{}"},
		{"resource", "", "<resource>"},
		{"resource", "resource id='5' type='stream'", "resource id='5' type='stream'"},
		{"resource", strings.Repeat("r", 70), strings.Repeat("r", 57) + "..."},
		{"string", strings.Repeat("s", 60), `"` + strings.Repeat("s", 60) + `"`},
		{"string", strings.Repeat("s", 61), `"` + strings.Repeat("s", 55) + `..."`},
	}
	for _, tt := range tests {
		if got := formatSimpleValue(tt.typ, tt.content, ""); got != tt.want {
			t.Errorf("formatSimpleValue(%q, %q) = %q, want %q", tt.typ, tt.content, got, tt.want)
		}
	}
}

func TestFormatSimpleValueTruncatesOnRuneBoundary(t *testing.T) {
	got := formatSimpleValue("string", strings.Repeat("é", 40), "")
	if !utf8.ValidString(got) {
		t.Errorf("truncated value is not valid UTF-8: %q", got)
	}
}

func TestFormatPropertyValueInvalidBase64(t *testing.T) {
	got := formatPropertyValue(Property{Type: "string", Encoding: "base64", Value: "not base64!"})
	if got != `"not base64!"` {
		t.Errorf("got %q", got)
	}
}

func TestFormatVariable(t *testing.T) {
	tests := []struct {
		v    Variable
		want string
	}{
		{Variable{Name: "$a", Type: "int", Value: "1"}, "$a" + strings.Repeat(" ", 29) + "int      = 1"},
		{Variable{Name: "$a[0]", Type: "int", Value: "1", Level: 1}, "  $a[0]" + strings.Repeat(" ", 24) + "int      = 1"},
		{Variable{Name: "$x", Type: "int", Value: "1", Level: 20}, strings.Repeat("  ", 20) + "$x       int      = 1"},
	}
	for _, tt := range tests {
		if got := FormatVariable(tt.v); got != tt.want {
			t.Errorf("FormatVariable(%+v) = %q, want %q", tt.v, got, tt.want)
		}
	}
}
