package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	dbgp "github.com/php-debugger/dbgp-client"
)

// Inspection tools: the call stack and the variables of a stopped session.

func (t *tools) addInspectTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:         "stack",
		Description:  "Show the call stack of a stopped session, innermost first.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		OutputSchema: outputSchema[stackOutput]("frames"),
	}, t.stack)
	mcp.AddTool(server, &mcp.Tool{
		Name: "variables",
		Description: "List the variables of a stack frame of a stopped session. Arrays and objects are " +
			"listed without their contents, with their size or class: use variable to look inside one.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		OutputSchema: outputSchema[variablesOutput]("variables"),
	}, t.variables)
	mcp.AddTool(server, &mcp.Tool{
		Name: "variable",
		Description: "Show one variable, or a path into one such as $user[\"name\"] or $obj->cache, with one page " +
			"of its elements or properties. Each is listed without its own contents; call variable again to go deeper.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		OutputSchema: outputSchema[variableOutput]("children"),
	}, t.variable)
	mcp.AddTool(server, &mcp.Tool{
		Name: "variable_value",
		Description: "Show the whole value of a variable whose value variables or variable returned truncated, " +
			"such as a long string.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.variableValue)
}

type frameInfo struct {
	Depth    int    `json:"depth" jsonschema:"the frame's depth: pass it as depth to variables and variable"`
	Function string `json:"function" jsonschema:"the function or method, or {main} for the script itself"`
	File     string `json:"file" jsonschema:"local path of the file"`
	Line     int    `json:"line" jsonschema:"the line being executed in this frame"`
}

type stackOutput struct {
	Session int         `json:"session" jsonschema:"the session id"`
	Frames  []frameInfo `json:"frames" jsonschema:"innermost first: depth 0 is where execution stopped"`
}

func (t *tools) stack(_ context.Context, _ *mcp.CallToolRequest, in sessionInput) (*mcp.CallToolResult, stackOutput, error) {
	s, err := t.session(in.Session)
	if err != nil {
		return nil, stackOutput{}, err
	}
	frames, err := s.GetStack()
	if err != nil {
		return nil, stackOutput{}, sessionError(s, err)
	}
	out := stackOutput{Session: s.ID(), Frames: []frameInfo{}}
	for _, f := range frames {
		out.Frames = append(out.Frames, frameInfo{Depth: f.Level, Function: f.Where, File: f.Filename, Line: f.Lineno})
	}
	return nil, out, nil
}

// frameInput picks where to look for variables.
type frameInput struct {
	Session int `json:"session,omitempty" jsonschema:"the session id; defaults to the newest session that has not ended"`
	Depth   int `json:"depth,omitempty" jsonschema:"the stack frame: 0 (the default) is where execution stopped, 1 its caller, and so on; see stack"`
	Context int `json:"context,omitempty" jsonschema:"0 (the default) local variables, 1 superglobals such as $_SERVER and $_GET, 2 user-defined constants"`
}

type variableInfo struct {
	Name      string `json:"name" jsonschema:"the full name, e.g. $user[\"name\"] or $obj->cache; pass it to variable to look inside an array or object"`
	Type      string `json:"type" jsonschema:"int, float, string, bool, null, array, object, resource, or unset for a variable without a value yet"`
	Value     string `json:"value,omitempty" jsonschema:"the value, for strings, numbers, booleans and resources"`
	Class     string `json:"class,omitempty" jsonschema:"the class, for objects"`
	Size      *int   `json:"size,omitempty" jsonschema:"the number of elements or properties, for arrays and objects"`
	Facet     string `json:"facet,omitempty" jsonschema:"public, protected or private, for object properties"`
	Truncated bool   `json:"truncated,omitempty" jsonschema:"only the start of the value was sent: call variable_value for all of it"`
}

// toVariable describes a property without its contents.
func toVariable(p dbgp.Property) variableInfo {
	v := variableInfo{Name: p.FullName, Type: p.Type, Facet: p.Facet}
	if v.Name == "" {
		v.Name = p.Name
	}
	switch p.Type {
	case "array", "object":
		size := p.NumChildren
		v.Size, v.Class = &size, p.ClassName
	case "uninitialized":
		v.Type = "unset"
	case "null":
	case "bool":
		v.Value = "false"
		if p.Value == "1" || p.Value == "true" {
			v.Value = "true"
		}
	default:
		value, err := p.DecodedValue()
		if err != nil {
			value = p.Value
		}
		v.Value, v.Truncated = value, p.Truncated()
	}
	return v
}

// inspectError explains an error from looking up a variable.
func inspectError(s *dbgp.Session, name string, depth int, err error) error {
	var engineErr *dbgp.EngineError
	if errors.As(err, &engineErr) && engineErr.Code == 300 {
		return fmt.Errorf("%s does not exist in frame %d", name, depth)
	}
	return sessionError(s, err)
}

type variablesOutput struct {
	Session   int            `json:"session" jsonschema:"the session id"`
	Depth     int            `json:"depth" jsonschema:"the stack frame"`
	Context   int            `json:"context" jsonschema:"0 local variables, 1 superglobals, 2 user-defined constants"`
	Variables []variableInfo `json:"variables" jsonschema:"the variables, without the contents of arrays and objects"`
}

func (t *tools) variables(_ context.Context, _ *mcp.CallToolRequest, in frameInput) (*mcp.CallToolResult, variablesOutput, error) {
	s, err := t.session(in.Session)
	if err != nil {
		return nil, variablesOutput{}, err
	}
	props, err := s.GetContextProperties(in.Depth, in.Context)
	if err != nil {
		return nil, variablesOutput{}, sessionError(s, err)
	}
	out := variablesOutput{Session: s.ID(), Depth: in.Depth, Context: in.Context, Variables: []variableInfo{}}
	for _, p := range props {
		out.Variables = append(out.Variables, toVariable(p))
	}
	return nil, out, nil
}

type variableInput struct {
	Session int    `json:"session,omitempty" jsonschema:"the session id; defaults to the newest session that has not ended"`
	Name    string `json:"name" jsonschema:"the variable, or a path into one, e.g. $user, $user[\"name\"] or $obj->cache"`
	Depth   int    `json:"depth,omitempty" jsonschema:"the stack frame: 0 (the default) is where execution stopped, 1 its caller, and so on; see stack"`
	Context int    `json:"context,omitempty" jsonschema:"0 (the default) local variables, 1 superglobals such as $_SERVER and $_GET, 2 user-defined constants"`
	Page    int    `json:"page,omitempty" jsonschema:"the page of elements or properties, from 0 (the default); see pages"`
}

type variableOutput struct {
	Variable variableInfo   `json:"variable" jsonschema:"the variable itself"`
	Children []variableInfo `json:"children" jsonschema:"one page of its elements or properties, for arrays and objects, each without its own contents"`
	Page     int            `json:"page" jsonschema:"this page, from 0"`
	Pages    int            `json:"pages" jsonschema:"how many pages of elements or properties there are; call variable with page for the others"`
}

func (t *tools) variable(_ context.Context, _ *mcp.CallToolRequest, in variableInput) (*mcp.CallToolResult, variableOutput, error) {
	s, err := t.session(in.Session)
	if err != nil {
		return nil, variableOutput{}, err
	}
	if in.Name == "" {
		return nil, variableOutput{}, errors.New("name is required, e.g. $user")
	}
	if in.Page < 0 {
		return nil, variableOutput{}, errors.New("page must not be negative")
	}
	p, err := s.GetProperty(in.Name, dbgp.PropertyOptions{Depth: in.Depth, Context: in.Context, Page: in.Page})
	if err != nil {
		return nil, variableOutput{}, inspectError(s, in.Name, in.Depth, err)
	}
	if in.Page >= p.Pages() {
		return nil, variableOutput{}, fmt.Errorf("%s has %d pages, numbered from 0", in.Name, p.Pages())
	}
	out := variableOutput{Variable: toVariable(*p), Children: []variableInfo{}, Page: in.Page, Pages: p.Pages()}
	for _, c := range p.ChildProperties {
		out.Children = append(out.Children, toVariable(c))
	}
	return nil, out, nil
}

type variableValueInput struct {
	Session int    `json:"session,omitempty" jsonschema:"the session id; defaults to the newest session that has not ended"`
	Name    string `json:"name" jsonschema:"the variable, or a path into one, e.g. $html or $page[\"body\"]"`
	Depth   int    `json:"depth,omitempty" jsonschema:"the stack frame: 0 (the default) is where execution stopped, 1 its caller, and so on; see stack"`
	Context int    `json:"context,omitempty" jsonschema:"0 (the default) local variables, 1 superglobals such as $_SERVER and $_GET, 2 user-defined constants"`
}

type variableValueOutput struct {
	Name  string `json:"name" jsonschema:"the variable"`
	Value string `json:"value" jsonschema:"its whole value"`
}

func (t *tools) variableValue(_ context.Context, _ *mcp.CallToolRequest, in variableValueInput) (*mcp.CallToolResult, variableValueOutput, error) {
	s, err := t.session(in.Session)
	if err != nil {
		return nil, variableValueOutput{}, err
	}
	if in.Name == "" {
		return nil, variableValueOutput{}, errors.New("name is required, e.g. $html")
	}
	value, err := s.GetPropertyValue(in.Name, dbgp.PropertyOptions{Depth: in.Depth, Context: in.Context})
	if err != nil {
		return nil, variableValueOutput{}, inspectError(s, in.Name, in.Depth, err)
	}
	return nil, variableValueOutput{Name: in.Name, Value: value}, nil
}
