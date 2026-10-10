package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	dbgp "github.com/php-debugger/dbgp-client"
)

// Source and output tools: the code around a location, what the script
// printed, and the PHP warnings it raised.

// How many lines source shows on each side of a line.
const (
	defaultAround = 5
	maxAround     = 100
)

func (t *tools) addOutputTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "source",
		Description: "Show numbered source lines around where a session stopped, or around another file and line. " +
			"Read through PHP, so it also works for PHP in a container or on another machine.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		OutputSchema: outputSchema[sourceOutput]("lines"),
	}, t.source)
	mcp.AddTool(server, &mcp.Tool{
		Name: "output",
		Description: "Show what a session's script has printed since the last call, so nothing is shown twice; " +
			"all returns everything kept so far. At most 32 KB is returned at a time: when more is set, call again for the rest. " +
			"Works after the session has ended.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.output)
	mcp.AddTool(server, &mcp.Tool{
		Name: "warnings",
		Description: "Show the PHP warnings, notices and other errors a session's script has raised since the last call; " +
			"all returns every one kept so far. The same warning raised again from the same line is listed once, with a count. " +
			"At most 50 are returned at a time: when more is set, call again for the rest. Works after the session has ended.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		OutputSchema: outputSchema[warningsOutput]("warnings"),
	}, t.warnings)
}

// latestSession returns the session with the given id, or by default the
// newest session, even one that has ended: what it printed is still there.
func (t *tools) latestSession(id int) (*dbgp.Session, error) {
	if id != 0 {
		return t.session(id)
	}
	sessions := t.srv.Sessions()
	if len(sessions) == 0 {
		return nil, errors.New("no session: call listen, start the PHP code, then wait_for_session")
	}
	return sessions[len(sessions)-1], nil
}

type sourceInput struct {
	Session int    `json:"session,omitempty" jsonschema:"the session id; defaults to the newest session that has not ended"`
	File    string `json:"file,omitempty" jsonschema:"the file, as a path on this machine; defaults to where the session stopped, or its script"`
	Line    int    `json:"line,omitempty" jsonschema:"the line to show the code around; defaults to where the session stopped, or 1 for another file"`
	Around  int    `json:"around,omitempty" jsonschema:"how many lines to show before and after the line: 5 by default, at most 100"`
}

type sourceLine struct {
	Line      int    `json:"line" jsonschema:"the line number"`
	Text      string `json:"text" jsonschema:"the line's code"`
	Current   bool   `json:"current,omitempty" jsonschema:"set on the line where execution is stopped"`
	Truncated bool   `json:"truncated,omitempty" jsonschema:"the line is longer than 500 bytes and only its start is shown"`
}

type sourceOutput struct {
	File  string       `json:"file" jsonschema:"local path of the file"`
	Lines []sourceLine `json:"lines" jsonschema:"the source lines, in order"`
}

func (t *tools) source(_ context.Context, _ *mcp.CallToolRequest, in sourceInput) (*mcp.CallToolResult, sourceOutput, error) {
	s, err := t.session(in.Session)
	if err != nil {
		return nil, sourceOutput{}, err
	}
	around := in.Around
	switch {
	case around == 0:
		around = defaultAround
	case around < 0 || around > maxAround:
		return nil, sourceOutput{}, fmt.Errorf("around must be between 1 and %d", maxAround)
	}

	st := s.State()
	file, line := in.File, in.Line
	if file == "" {
		file = st.File
		if line == 0 {
			line = st.Line
		}
		if file == "" {
			file = s.Script()
		}
	} else if file, err = absolutePath(file); err != nil {
		return nil, sourceOutput{}, err
	}
	if line <= 0 {
		line = 1
	}

	begin := max(1, line-around)
	text, err := s.GetSource(file, begin, line+around)
	if err != nil {
		var engineErr *dbgp.EngineError
		if errors.As(err, &engineErr) && engineErr.Code == 100 {
			return nil, sourceOutput{}, fmt.Errorf("PHP cannot read %s", file)
		}
		return nil, sourceOutput{}, sessionError(s, err)
	}
	out := sourceOutput{File: file, Lines: []sourceLine{}}
	if text == "" {
		return nil, out, nil
	}
	for i, code := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		n := begin + i
		short := cutText(code, maxLineBytes)
		out.Lines = append(out.Lines, sourceLine{
			Line: n, Text: short, Truncated: len(short) < len(code),
			Current: st.Status == dbgp.StatusBreak && st.File == file && st.Line == n,
		})
	}
	return nil, out, nil
}

type readInput struct {
	Session int  `json:"session,omitempty" jsonschema:"the session id; defaults to the newest session, even one that has ended"`
	All     bool `json:"all,omitempty" jsonschema:"return everything kept so far, not only what is new since the last call"`
}

// readFrom runs read from where the last read of a session's output or
// warnings ended, or from the start for all, and records where it ends.
func (t *tools) readFrom(positions *map[int]int, id int, all bool, read func(from int) int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if *positions == nil {
		*positions = map[int]int{}
	}
	from := (*positions)[id]
	if all {
		from = 0
	}
	(*positions)[id] = read(from)
}

type outputOutput struct {
	Session   int    `json:"session" jsonschema:"the session id"`
	Output    string `json:"output" jsonschema:"what the script printed; empty if nothing new"`
	Truncated bool   `json:"truncated,omitempty" jsonschema:"earlier output was dropped: only the most recent 1 MB is kept"`
	More      bool   `json:"more,omitempty" jsonschema:"there is more output after this: call output again for it"`
}

func (t *tools) output(_ context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, outputOutput, error) {
	s, err := t.latestSession(in.Session)
	if err != nil {
		return nil, outputOutput{}, err
	}
	out := outputOutput{Session: s.ID()}
	t.readFrom(&t.outputFrom, s.ID(), in.All, func(from int) int {
		text, next, truncated := s.Output(from)
		out.Output, out.Truncated = cutText(text, maxTextBytes), truncated
		out.More = len(out.Output) < len(text)
		return next - (len(text) - len(out.Output))
	})
	return nil, out, nil
}

type warningInfo struct {
	Type    string `json:"type" jsonschema:"the kind of error, e.g. Warning, Notice, Deprecated or Fatal error"`
	Message string `json:"message" jsonschema:"PHP's message"`
	File    string `json:"file" jsonschema:"local path of the file where it was raised"`
	Line    int    `json:"line" jsonschema:"the line where it was raised"`
	Count   int    `json:"count" jsonschema:"how many times it was raised"`
}

type warningsOutput struct {
	Session  int           `json:"session" jsonschema:"the session id"`
	Warnings []warningInfo `json:"warnings" jsonschema:"the warnings, notices and errors, in the order first raised; empty if nothing new"`
	More     bool          `json:"more,omitempty" jsonschema:"there are more after these: call warnings again for them"`
}

func (t *tools) warnings(_ context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, warningsOutput, error) {
	s, err := t.latestSession(in.Session)
	if err != nil {
		return nil, warningsOutput{}, err
	}
	out := warningsOutput{Session: s.ID(), Warnings: []warningInfo{}}
	t.readFrom(&t.warningsFrom, s.ID(), in.All, func(from int) int {
		notes, next := s.Notifications(from)
		seen := map[warningInfo]int{} // index in out.Warnings, by warning with no count
		for i, n := range notes {
			// Other notifications, such as breakpoint_resolved, are routine.
			if n.Name != "error" || n.Message == nil {
				continue
			}
			w := warningInfo{
				Type: n.Message.Type, Message: strings.TrimSpace(n.Message.Text),
				File: n.Message.Filename, Line: n.Message.Lineno,
			}
			if j, ok := seen[w]; ok {
				out.Warnings[j].Count++
				continue
			}
			if len(out.Warnings) == maxListItems {
				// The next call starts at this one.
				out.More = true
				return next - (len(notes) - i)
			}
			seen[w] = len(out.Warnings)
			w.Count = 1
			out.Warnings = append(out.Warnings, w)
		}
		return next
	})
	return nil, out, nil
}
