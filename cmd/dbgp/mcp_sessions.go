package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	dbgp "github.com/php-debugger/dbgp-client"
)

// Session tools: waiting for PHP to connect, running and stepping, and
// ending sessions.

// Waits are chosen by the agent, within these limits; MCP clients give up
// on tool calls that take too long.
const (
	defaultWait = 30 * time.Second
	maxWait     = 600 * time.Second
)

// continueCommands are the commands the continue tool accepts.
var continueCommands = []any{dbgp.ContinueRun, dbgp.ContinueStepInto, dbgp.ContinueStepOver, dbgp.ContinueStepOut}

func (t *tools) addSessionTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:         "sessions",
		Description:  "List the debugging sessions: one per PHP script run or request that connected, oldest first, including ended ones.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		OutputSchema: outputSchema[sessionsOutput]("sessions"),
	}, t.sessions)
	mcp.AddTool(server, &mcp.Tool{
		Name: "wait_for_session",
		Description: "Wait for PHP to connect and return the session, stopped at the start of the script. " +
			"Returns the oldest session this tool has not returned yet, so one that connected earlier is not missed. " +
			"Call listen first, then start the PHP code.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.waitForSession)
	mcp.AddTool(server, &mcp.Tool{
		Name: "continue",
		Description: "Resume a session: run to the next breakpoint or the end of the script, or step one statement. " +
			"Waits until the script stops and returns where. If it is still running when the wait ends, " +
			"the status is running: call wait_for_stop to keep waiting.",
		InputSchema: continueSchema(),
	}, t.continueSession)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "wait_for_stop",
		Description: "Keep waiting for a running session to stop, after continue returned with status running.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.waitForStop)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "stop",
		Description: "End a session: the script stops where it is.",
	}, t.stopSession)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "detach",
		Description: "Stop debugging a session and let the script run on to its end without the debugger.",
	}, t.detachSession)
}

// sessionList returns the server's sessions, never nil.
func (t *tools) sessionList() []sessionInfo {
	list := []sessionInfo{}
	for _, s := range t.srv.Sessions() {
		list = append(list, toSessionInfo(s))
	}
	return list
}

func toSessionInfo(s *dbgp.Session) sessionInfo {
	st := s.State()
	info := sessionInfo{ID: s.ID(), Script: s.Script(), Status: st.Status}
	if ended(st) {
		info.Status = "ended"
	} else if st.Status == dbgp.StatusBreak {
		info.File, info.Line = st.File, st.Line
	}
	return info
}

// session returns the session with the given id, or by default the newest
// session that has not ended.
func (t *tools) session(id int) (*dbgp.Session, error) {
	if id != 0 {
		if s := t.srv.Session(id); s != nil {
			return s, nil
		}
		return nil, fmt.Errorf("no session %d", id)
	}
	sessions := t.srv.Sessions()
	for i := len(sessions) - 1; i >= 0; i-- {
		if !ended(sessions[i].State()) {
			return sessions[i], nil
		}
	}
	return nil, errors.New("no session: call listen, start the PHP code, then wait_for_session")
}

// waitDuration converts a wait in seconds, 0 meaning the default.
func waitDuration(seconds float64) (time.Duration, error) {
	switch {
	case seconds < 0:
		return 0, errors.New("wait must not be negative")
	case seconds == 0:
		return defaultWait, nil
	case seconds > maxWait.Seconds():
		return 0, fmt.Errorf("wait must be at most %v seconds", maxWait.Seconds())
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

type sessionsOutput struct {
	Sessions []sessionInfo `json:"sessions" jsonschema:"debugging sessions, oldest first, including ended ones"`
}

func (t *tools) sessions(context.Context, *mcp.CallToolRequest, noInput) (*mcp.CallToolResult, sessionsOutput, error) {
	return nil, sessionsOutput{Sessions: t.sessionList()}, nil
}

type waitInput struct {
	Wait float64 `json:"wait,omitempty" jsonschema:"seconds to wait, 30 by default and at most 600"`
}

type waitForSessionOutput struct {
	Connected bool         `json:"connected" jsonschema:"whether a session connected before the wait ended"`
	Session   *sessionInfo `json:"session,omitempty" jsonschema:"the session, when one connected"`
}

func (t *tools) waitForSession(ctx context.Context, _ *mcp.CallToolRequest, in waitInput) (*mcp.CallToolResult, waitForSessionOutput, error) {
	wait, err := waitDuration(in.Wait)
	if err != nil {
		return nil, waitForSessionOutput{}, err
	}
	if !t.srv.Listening() {
		// Nothing can connect; only a session that already connected can be returned.
		done, cancel := context.WithCancel(ctx)
		cancel()
		if s, err := t.srv.WaitForSession(done); err == nil {
			info := toSessionInfo(s)
			return nil, waitForSessionOutput{Connected: true, Session: &info}, nil
		}
		return nil, waitForSessionOutput{}, errors.New("not listening for PHP connections: call listen first")
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	s, err := t.srv.WaitForSession(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, waitForSessionOutput{Connected: false}, nil
	}
	if err != nil {
		return nil, waitForSessionOutput{}, err
	}
	info := toSessionInfo(s)
	return nil, waitForSessionOutput{Connected: true, Session: &info}, nil
}

type sessionInput struct {
	Session int `json:"session,omitempty" jsonschema:"the session id; defaults to the newest session that has not ended"`
}

type continueInput struct {
	Session int     `json:"session,omitempty" jsonschema:"the session id; defaults to the newest session that has not ended"`
	Command string  `json:"command,omitempty" jsonschema:"run (the default) runs to the next breakpoint or the end of the script; step_into, step_over and step_out step one statement, into, over or out of a function call"`
	Wait    float64 `json:"wait,omitempty" jsonschema:"seconds to wait for the script to stop, 30 by default and at most 600"`
}

// continueSchema is continueInput's schema, listing the commands.
func continueSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[continueInput](nil)
	if err != nil {
		panic(fmt.Sprintf("continue schema: %v", err))
	}
	schema.Properties["command"].Enum = continueCommands
	return schema
}

type stateOutput struct {
	Session          int    `json:"session" jsonschema:"the session id"`
	Status           string `json:"status" jsonschema:"break: stopped, see file and line; running: still running when the wait ended, call wait_for_stop; stopping: the script finished, call stop to end the session; starting: not started yet; ended: the session is over"`
	File             string `json:"file,omitempty" jsonschema:"local path of the file where execution stopped, when status is break"`
	Line             int    `json:"line,omitempty" jsonschema:"line where execution stopped, when status is break"`
	Exception        string `json:"exception,omitempty" jsonschema:"exception class, when stopped on an exception breakpoint"`
	ExceptionMessage string `json:"exceptionMessage,omitempty" jsonschema:"the exception's message, when stopped on an exception breakpoint"`
}

func toState(s *dbgp.Session, st dbgp.State) stateOutput {
	out := stateOutput{Session: s.ID(), Status: st.Status}
	switch {
	case ended(st):
		out.Status = "ended"
	case st.Status == dbgp.StatusBreak:
		out.File, out.Line = st.File, st.Line
		out.Exception, out.ExceptionMessage = st.Exception, st.Message
	}
	return out
}

// sessionError explains a session error in terms of the tools to call.
func sessionError(s *dbgp.Session, err error) error {
	switch {
	case errors.Is(err, dbgp.ErrRunning):
		return fmt.Errorf("session %d is still running: call wait_for_stop first", s.ID())
	case ended(s.State()):
		return fmt.Errorf("session %d has ended", s.ID())
	}
	return err
}

func (t *tools) continueSession(ctx context.Context, _ *mcp.CallToolRequest, in continueInput) (*mcp.CallToolResult, stateOutput, error) {
	s, err := t.session(in.Session)
	if err != nil {
		return nil, stateOutput{}, err
	}
	wait, err := waitDuration(in.Wait)
	if err != nil {
		return nil, stateOutput{}, err
	}
	command := in.Command
	if command == "" {
		command = dbgp.ContinueRun
	}
	st, err := s.Continue(ctx, command, wait)
	if err != nil {
		return nil, stateOutput{}, sessionError(s, err)
	}
	return nil, toState(s, st), nil
}

type waitForStopInput struct {
	Session int     `json:"session,omitempty" jsonschema:"the session id; defaults to the newest session that has not ended"`
	Wait    float64 `json:"wait,omitempty" jsonschema:"seconds to wait for the script to stop, 30 by default and at most 600"`
}

func (t *tools) waitForStop(ctx context.Context, _ *mcp.CallToolRequest, in waitForStopInput) (*mcp.CallToolResult, stateOutput, error) {
	s, err := t.session(in.Session)
	if err != nil {
		return nil, stateOutput{}, err
	}
	wait, err := waitDuration(in.Wait)
	if err != nil {
		return nil, stateOutput{}, err
	}
	st, err := s.Wait(ctx, wait)
	if err != nil {
		return nil, stateOutput{}, err
	}
	return nil, toState(s, st), nil
}

func (t *tools) stopSession(_ context.Context, _ *mcp.CallToolRequest, in sessionInput) (*mcp.CallToolResult, stateOutput, error) {
	s, err := t.session(in.Session)
	if err != nil {
		return nil, stateOutput{}, err
	}
	if err := s.Stop(); err != nil {
		return nil, stateOutput{}, sessionError(s, err)
	}
	return nil, toState(s, s.State()), nil
}

func (t *tools) detachSession(_ context.Context, _ *mcp.CallToolRequest, in sessionInput) (*mcp.CallToolResult, stateOutput, error) {
	s, err := t.session(in.Session)
	if err != nil {
		return nil, stateOutput{}, err
	}
	if err := s.Detach(); err != nil {
		return nil, stateOutput{}, sessionError(s, err)
	}
	return nil, toState(s, s.State()), nil
}
