// Package dbgp implements the DBGp protocol for PHP debugging
package dbgp

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// charsetReader handles non-UTF-8 XML encodings
func charsetReader(encoding string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(encoding) {
	case "utf-8", "us-ascii":
		return input, nil
	case "iso-8859-1", "latin1":
		return decodeSingleByte(input, nil)
	case "windows-1252":
		return decodeSingleByte(input, windows1252Overrides)
	default:
		return nil, fmt.Errorf("unsupported charset: %s", encoding)
	}
}

func decodeSingleByte(input io.Reader, overrides map[byte]rune) (io.Reader, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}

	runes := make([]rune, len(data))
	for i, b := range data {
		if overrides != nil {
			if mapped, ok := overrides[b]; ok {
				runes[i] = mapped
				continue
			}
		}
		runes[i] = rune(b)
	}

	return strings.NewReader(string(runes)), nil
}

var windows1252Overrides = map[byte]rune{
	0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„', 0x85: '…', 0x86: '†', 0x87: '‡', 0x88: 'ˆ',
	0x89: '‰', 0x8A: 'Š', 0x8B: '‹', 0x8C: 'Œ', 0x8E: 'Ž', 0x91: '‘', 0x92: '’', 0x93: '“',
	0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—', 0x98: '˜', 0x99: '™', 0x9A: 'š', 0x9B: '›',
	0x9C: 'œ', 0x9E: 'ž', 0x9F: 'Ÿ',
}

// Status values
const (
	StatusStarting = "starting"
	StatusStopping = "stopping"
	StatusStopped  = "stopped"
	StatusRunning  = "running"
	StatusBreak    = "break"
	StatusDetached = "detached"
)

// Breakpoint types
const (
	BreakpointLine        = "line"
	BreakpointConditional = "conditional"
	BreakpointCall        = "call"
	BreakpointReturn      = "return"
	BreakpointException   = "exception"
	BreakpointWatch       = "watch"
)

// InitPacket is sent by PHP when connection is established
type InitPacket struct {
	XMLName  xml.Name `xml:"init"`
	AppID    string   `xml:"appid,attr"`
	IDEKey   string   `xml:"idekey,attr"`
	Session  string   `xml:"session,attr"`
	Thread   string   `xml:"thread,attr"`
	Parent   string   `xml:"parent,attr"`
	Language string   `xml:"language,attr"`
	Protocol string   `xml:"protocol_version,attr"`
	FileURI  string   `xml:"fileuri,attr"`
	Engine   Engine   `xml:"engine"`
	// EngineVersion is Engine.Version, kept for compatibility.
	EngineVersion string `xml:"-"`
}

// Engine identifies the debugger engine, e.g. PHP Debugger or Xdebug.
type Engine struct {
	Name    string `xml:",chardata"`
	Version string `xml:"version,attr"`
}

// Response is the generic DBGp response
type Response struct {
	XMLName      xml.Name `xml:"response"`
	Command      string   `xml:"command,attr"`
	Transaction  int      `xml:"transaction_id,attr"`
	Status       string   `xml:"status,attr,omitempty"`
	Reason       string   `xml:"reason,attr,omitempty"`
	Success      string   `xml:"success,attr,omitempty"`
	BreakpointID int      `xml:"id,attr,omitempty"`
	Encoding     string   `xml:"encoding,attr,omitempty"`

	// For feature_get: "0" when the engine does not support the feature
	FeatureName string `xml:"feature_name,attr,omitempty"`
	Supported   string `xml:"supported,attr,omitempty"`

	// For breakpoint_get, breakpoint_list, breakpoint_remove
	Breakpoints []BreakpointInfo `xml:"breakpoint,omitempty"`

	// For context_get, property_get and eval
	Context    int        `xml:"context,attr,omitempty"`
	Properties []Property `xml:"property,omitempty"`

	// For property_value
	Type string `xml:"type,attr,omitempty"`
	Size int    `xml:"size,attr,omitempty"`

	// For context_names
	Contexts []ContextName `xml:"context,omitempty"`

	// For typemap_get
	TypeMap []TypeMapEntry `xml:"map,omitempty"`

	// For stack_get and stack_depth
	Stack []StackFrame `xml:"stack,omitempty"`
	Depth int          `xml:"depth,attr,omitempty"`

	// For errors
	Error *Error `xml:"error,omitempty"`

	// Message for breakpoint hit
	Message *Message `xml:"https://xdebug.org/dbgp/xdebug message,omitempty"`

	// Raw for debugging
	Raw string `xml:",innerxml"`
	// Value contains raw response chardata when present
	Value string `xml:",chardata"`
}

// Error represents a DBGp error
type Error struct {
	Code    int    `xml:"code,attr"`
	Message string `xml:"message"`
}

// Message is the xdebug:message element: where execution stopped, or the
// PHP error an "error" notification reports. Exception, Type, Code and Text
// are set for exceptions and errors.
type Message struct {
	Filename  string `xml:"filename,attr"`
	Lineno    int    `xml:"lineno,attr"`
	Exception string `xml:"exception,attr,omitempty"`
	Type      string `xml:"type,attr,omitempty"`
	Code      string `xml:"code,attr,omitempty"`
	Text      string `xml:",chardata"`
}

// Notification is an asynchronous notify packet from the engine, such as
// "error" (a PHP warning or notice) or "breakpoint_resolved".
type Notification struct {
	XMLName    xml.Name        `xml:"notify"`
	Name       string          `xml:"name,attr"`
	Message    *Message        `xml:"https://xdebug.org/dbgp/xdebug message"`
	Breakpoint *BreakpointInfo `xml:"breakpoint"`
}

// Stream is program output the engine forwards (see the stdout command).
type Stream struct {
	XMLName  xml.Name `xml:"stream"`
	Type     string   `xml:"type,attr"`
	Encoding string   `xml:"encoding,attr"`
	Value    string   `xml:",chardata"`
}

// BreakpointInfo contains breakpoint details
type BreakpointInfo struct {
	ID           int    `xml:"id,attr"`
	Type         string `xml:"type,attr"`
	Filename     string `xml:"filename,attr"`
	Lineno       int    `xml:"lineno,attr"`
	Function     string `xml:"function,attr,omitempty"`
	State        string `xml:"state,attr"`
	Resolved     string `xml:"resolved,attr,omitempty"`
	Exception    string `xml:"exception,attr,omitempty"`
	HitCount     int    `xml:"hit_count,attr,omitempty"`
	HitValue     int    `xml:"hit_value,attr,omitempty"`
	HitCondition string `xml:"hit_condition,attr,omitempty"`
	Temporary    int    `xml:"temporary,attr,omitempty"`
	// Expression is the decoded condition of a conditional breakpoint.
	Expression string `xml:"-"`
}

// UnmarshalXML reads the expression from its <expression> child element
// (base64-encoded by Xdebug and PHP Debugger), or from an expression
// attribute as some engines send it.
func (b *BreakpointInfo) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type plain BreakpointInfo // drops this method, avoiding recursion
	var raw struct {
		plain
		ExpressionAttr string `xml:"expression,attr"`
		ExpressionElem *struct {
			Encoding string `xml:"encoding,attr"`
			Value    string `xml:",chardata"`
		} `xml:"expression"`
	}
	if err := d.DecodeElement(&raw, &start); err != nil {
		return err
	}
	*b = BreakpointInfo(raw.plain)
	b.Expression = raw.ExpressionAttr
	if e := raw.ExpressionElem; e != nil {
		expr, err := decodeEncoded(e.Encoding, e.Value)
		if err != nil {
			return fmt.Errorf("breakpoint %d expression: %w", b.ID, err)
		}
		b.Expression = expr
	}
	return nil
}

// decodeEncoded decodes a value sent with the given DBGp encoding attribute.
func decodeEncoded(encoding, value string) (string, error) {
	switch encoding {
	case "", "none":
		return value, nil
	case "base64":
		cleaned := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, value)
		decoded, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return "", fmt.Errorf("decode base64: %w", err)
		}
		return string(decoded), nil
	default:
		return "", fmt.Errorf("unsupported encoding %q", encoding)
	}
}

// Property represents a variable
type Property struct {
	Name        string `xml:"name,attr"`
	FullName    string `xml:"fullname,attr"`
	Type        string `xml:"type,attr"`
	ClassName   string `xml:"classname,attr,omitempty"`
	Facet       string `xml:"facet,attr,omitempty"`
	Size        int    `xml:"size,attr,omitempty"` // full length of a string value
	Children    int    `xml:"children,attr,omitempty"`
	NumChildren int    `xml:"numchildren,attr,omitempty"`
	// Page and PageSize locate ChildProperties among all NumChildren.
	Page            int        `xml:"page,attr,omitempty"`
	PageSize        int        `xml:"pagesize,attr,omitempty"`
	Encoding        string     `xml:"encoding,attr,omitempty"`
	Value           string     `xml:",chardata"`
	ChildProperties []Property `xml:"property,omitempty"`
}

// DecodedValue returns the property's value with its encoding removed.
// Arrays and objects have no value of their own; see ChildProperties.
func (p Property) DecodedValue() (string, error) {
	return decodeEncoded(p.Encoding, p.Value)
}

// Truncated reports whether the engine sent only part of a string value
// (see the max_data feature); GetPropertyValue returns all of it.
func (p Property) Truncated() bool {
	value, err := p.DecodedValue()
	return err == nil && p.Size > len(value)
}

// Pages returns how many pages of children the property has.
func (p Property) Pages() int {
	if p.PageSize <= 0 || p.NumChildren == 0 {
		return 1
	}
	return (p.NumChildren + p.PageSize - 1) / p.PageSize
}

// ContextName is a variable context, such as Locals or Superglobals.
type ContextName struct {
	ID   int    `xml:"id,attr"`
	Name string `xml:"name,attr"`
}

// TypeMapEntry maps a language type to its DBGp type and XML Schema type.
type TypeMapEntry struct {
	Name       string // PHP type name, e.g. "int"
	Type       string // DBGp common type, e.g. "int" or "hash"
	SchemaType string // XML Schema type, e.g. "xsd:decimal"; may be empty
}

const xsiNamespace = "http://www.w3.org/2001/XMLSchema-instance"

// UnmarshalXML reads a map element. It has both type and xsi:type
// attributes, which plain struct tags cannot tell apart.
func (m *TypeMapEntry) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for _, a := range start.Attr {
		switch {
		case a.Name.Local == "name":
			m.Name = a.Value
		case a.Name.Local == "type" && a.Name.Space == "":
			m.Type = a.Value
		case a.Name.Local == "type" && a.Name.Space == xsiNamespace:
			m.SchemaType = a.Value
		}
	}
	return d.Skip()
}

// StackFrame represents a call stack entry
type StackFrame struct {
	Level    int    `xml:"level,attr"`
	Type     string `xml:"type,attr"`
	Filename string `xml:"filename,attr"`
	Lineno   int    `xml:"lineno,attr"`
	Where    string `xml:"where,attr"`
	Cmmd     string `xml:"cmmd,attr,omitempty"`
}

// ParseInit parses the init packet from PHP
func ParseInit(data []byte) (*InitPacket, error) {
	var init InitPacket
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charsetReader
	if err := decoder.Decode(&init); err != nil {
		return nil, fmt.Errorf("parse init: %w", err)
	}
	init.EngineVersion = init.Engine.Version
	return &init, nil
}

// ParseResponse parses a DBGp response
func ParseResponse(data []byte) (*Response, error) {
	var resp Response
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charsetReader
	if err := decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &resp, nil
}

// ParseNotification parses a notify packet
func ParseNotification(data []byte) (*Notification, error) {
	var n Notification
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charsetReader
	if err := decoder.Decode(&n); err != nil {
		return nil, fmt.Errorf("parse notification: %w", err)
	}
	if n.Message != nil {
		n.Message.Filename = FormatFileURI(n.Message.Filename)
	}
	return &n, nil
}

// ParseStream parses a stream packet and returns its decoded output
func ParseStream(data []byte) (*Stream, string, error) {
	var st Stream
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charsetReader
	if err := decoder.Decode(&st); err != nil {
		return nil, "", fmt.Errorf("parse stream: %w", err)
	}
	text, err := decodeEncoded(st.Encoding, st.Value)
	if err != nil {
		return nil, "", fmt.Errorf("parse stream: %w", err)
	}
	return &st, text, nil
}

// packetKind returns the root element name of a packet: "init",
// "response", "notify" or "stream".
func packetKind(data []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charsetReader
	for {
		tok, err := decoder.Token()
		if err != nil {
			return "", fmt.Errorf("read packet root: %w", err)
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local, nil
		}
	}
}

// ParseMessage extracts breakpoint hit info from response
func (r *Response) ParseMessage() (file string, line int) {
	if r.Message != nil {
		file = FormatFileURI(r.Message.Filename)
		line = r.Message.Lineno
	}
	return
}

// FormatStack formats the call stack for display
func FormatStack(frames []StackFrame) []string {
	lines := make([]string, len(frames))
	for i, f := range frames {
		file := FormatFileURI(f.Filename)
		lines[i] = fmt.Sprintf("#%d %s() at %s:%d", f.Level, f.Where, file, f.Lineno)
	}
	return lines
}

// FormatFileURI converts file:// URI to path
func FormatFileURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return strings.TrimPrefix(uri, "file://")
	}

	path, err := url.PathUnescape(u.Path)
	if err != nil {
		path = u.Path
	}

	if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}

	if u.Host != "" {
		return "//" + u.Host + filepath.FromSlash(path)
	}

	return filepath.FromSlash(path)
}

// MakeFileURI converts path to file:// URI
func MakeFileURI(path string) string {
	if strings.HasPrefix(path, "file://") {
		u, err := url.Parse(path)
		if err == nil {
			return u.String()
		}
		return path
	}

	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		trimmed := strings.TrimLeft(path, `/\`)
		parts := strings.SplitN(trimmed, `\`, 2)
		if len(parts) == 1 {
			parts = strings.SplitN(trimmed, "/", 2)
		}
		host := parts[0]
		sharePath := ""
		if len(parts) == 2 {
			sharePath = "/" + strings.ReplaceAll(parts[1], `\`, `/`)
		}
		return (&url.URL{Scheme: "file", Host: host, Path: sharePath}).String()
	}

	slashed := strings.ReplaceAll(path, `\`, `/`)
	slashed = filepath.ToSlash(slashed)
	if strings.HasPrefix(slashed, "/") {
		return (&url.URL{Scheme: "file", Path: slashed}).String()
	}

	return (&url.URL{Scheme: "file", Path: "/" + slashed}).String()
}

// ParseBreakpointSpec parses "file.php:42" or "file.php:42,55,60"
func ParseBreakpointSpec(spec string) (file string, lines []int, err error) {
	sep := strings.LastIndex(spec, ":")
	if sep <= 0 || sep == len(spec)-1 {
		return "", nil, fmt.Errorf("invalid breakpoint spec: %s (expected file:line)", spec)
	}

	file = spec[:sep]
	linePart := spec[sep+1:]
	for _, r := range linePart {
		if (r < '0' || r > '9') && r != ',' {
			return "", nil, fmt.Errorf("invalid breakpoint spec: %s (expected file:line)", spec)
		}
	}

	lineStrs := strings.Split(linePart, ",")
	lines = make([]int, 0, len(lineStrs))

	for _, ls := range lineStrs {
		l, err := strconv.Atoi(strings.TrimSpace(ls))
		if err != nil {
			return "", nil, fmt.Errorf("invalid line number: %s", ls)
		}
		lines = append(lines, l)
	}

	return file, lines, nil
}
