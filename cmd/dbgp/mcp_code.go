package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	dbgp "github.com/php-debugger/dbgp-client"
)

// Code tools: evaluating PHP and changing variables in the debugged
// script. They run code in it, so dbgp mcp -no-eval leaves them out.

func (t *tools) addCodeTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "eval",
		Description: "Evaluate a PHP expression where a session is stopped, and show the result like variable does: " +
			"an array or object with one page of its contents. To look deeper into the result, evaluate the part " +
			"you want, e.g. ($expr)[\"key\"]. The expression runs in the script, so it can change its state. " +
			"It always runs in the innermost frame, where execution stopped, and sees only that frame's variables: " +
			"PHP offers no way to evaluate in a caller's frame. To read a caller's variables, use variables, " +
			"variable or variable_value with depth.",
		OutputSchema: outputSchema[evalOutput]("children"),
	}, t.eval)
	mcp.AddTool(server, &mcp.Tool{
		Name: "set_variable",
		Description: "Set a variable, or a path into one such as $user[\"name\"], to the value of a PHP expression, " +
			"in a stack frame of a stopped session. Returns the variable as it is afterwards.",
	}, t.setVariable)
}

type evalInput struct {
	Session int    `json:"session,omitempty" jsonschema:"the session id; defaults to the newest session that has not ended"`
	Code    string `json:"code" jsonschema:"a PHP expression, e.g. count($items) or $user->getName(); it sees the variables of the frame where execution stopped"`
	Page    int    `json:"page,omitempty" jsonschema:"the page of the result's elements or properties, from 0 (the default); see pages"`
}

type evalOutput struct {
	Result   *variableInfo  `json:"result,omitempty" jsonschema:"the result, named after the expression; absent when it has no value"`
	Children []variableInfo `json:"children" jsonschema:"one page of the result's elements or properties, for arrays and objects"`
	Page     int            `json:"page" jsonschema:"this page, from 0"`
	Pages    int            `json:"pages" jsonschema:"how many pages of elements or properties the result has; call eval with page for the others"`
}

// codeError explains an error from running PHP code.
func codeError(s *dbgp.Session, err error) error {
	var engineErr *dbgp.EngineError
	if errors.As(err, &engineErr) && engineErr.Code == 206 {
		return fmt.Errorf("PHP could not evaluate it: %s", engineErr.Message)
	}
	return sessionError(s, err)
}

func (t *tools) eval(_ context.Context, _ *mcp.CallToolRequest, in evalInput) (*mcp.CallToolResult, evalOutput, error) {
	s, err := t.session(in.Session)
	if err != nil {
		return nil, evalOutput{}, err
	}
	if in.Code == "" {
		return nil, evalOutput{}, errors.New("code is required, e.g. count($items)")
	}
	if in.Page < 0 {
		return nil, evalOutput{}, errors.New("page must not be negative")
	}
	p, err := s.EvalProperty(in.Code, in.Page)
	if err != nil {
		return nil, evalOutput{}, codeError(s, err)
	}
	out := evalOutput{Children: []variableInfo{}, Page: in.Page, Pages: 1}
	if p == nil {
		return nil, out, nil
	}
	if in.Page >= p.Pages() {
		return nil, evalOutput{}, fmt.Errorf("the result has %d pages, numbered from 0", p.Pages())
	}
	result := toVariable(*p)
	result.Name = in.Code
	out.Result, out.Pages = &result, p.Pages()
	for _, c := range p.ChildProperties {
		out.Children = append(out.Children, toVariable(c))
	}
	return nil, out, nil
}

type setVariableInput struct {
	Session int    `json:"session,omitempty" jsonschema:"the session id; defaults to the newest session that has not ended"`
	Name    string `json:"name" jsonschema:"the variable, or a path into one, e.g. $count or $user[\"name\"]"`
	Value   string `json:"value" jsonschema:"a PHP expression for the new value, e.g. 42, 'text' or $a * 2"`
	Depth   int    `json:"depth,omitempty" jsonschema:"the stack frame: 0 (the default) is where execution stopped, 1 its caller, and so on; see stack"`
	Context int    `json:"context,omitempty" jsonschema:"0 (the default) local variables, 1 superglobals such as $_SERVER and $_GET"`
}

type setVariableOutput struct {
	Variable variableInfo `json:"variable" jsonschema:"the variable after it was set"`
}

func (t *tools) setVariable(_ context.Context, _ *mcp.CallToolRequest, in setVariableInput) (*mcp.CallToolResult, setVariableOutput, error) {
	s, err := t.session(in.Session)
	if err != nil {
		return nil, setVariableOutput{}, err
	}
	if in.Name == "" || in.Value == "" {
		return nil, setVariableOutput{}, errors.New("name and value are both required, e.g. $count and 42")
	}
	opts := dbgp.PropertyOptions{Depth: in.Depth, Context: in.Context}
	if err := s.SetProperty(in.Name, in.Value, opts); err != nil {
		var engineErr *dbgp.EngineError
		if errors.Is(err, dbgp.ErrRunning) || ended(s.State()) || errors.As(err, &engineErr) {
			return nil, setVariableOutput{}, codeError(s, err)
		}
		// The engine reports a failed assignment without a reason.
		return nil, setVariableOutput{}, fmt.Errorf("PHP could not set %s to %s: check that the value is a valid PHP expression", in.Name, in.Value)
	}
	p, err := s.GetProperty(in.Name, opts)
	if err != nil {
		return nil, setVariableOutput{}, inspectError(s, in.Name, in.Depth, err)
	}
	return nil, setVariableOutput{Variable: toVariable(*p)}, nil
}
