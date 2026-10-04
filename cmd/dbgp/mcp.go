package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	dbgp "github.com/php-debugger/dbgp-client"
)

// version identifies this build to MCP clients.
const version = "dev"

// mcpInstructions are sent to MCP clients when they connect.
const mcpInstructions = `dbgp debugs PHP scripts and requests through PHP Debugger or Xdebug.
It does not listen for PHP connections until the listen tool is called, so
PHP runs at full speed until you want to debug. Call status to see what is
going on, listen before starting the PHP code to debug, and unlisten when done.

Before starting PHP, check which debugger it has with php -v: it prints
"with PHP Debugger" or "with Xdebug". PHP Debugger is always in debug mode and
starts debugging every request by itself, so do not set xdebug.mode or
xdebug.start_with_request for it. Xdebug needs xdebug.mode=debug and
xdebug.start_with_request=yes.

PHP finds dbgp through xdebug.client_host and xdebug.client_port, which
default to localhost and 9003 in both debuggers. When PHP runs on this machine
and the listen tool's address is 127.0.0.1:9003, dbgp's default, those
defaults already reach dbgp and neither needs to be set. Otherwise set them to
the port from that address and a host PHP can reach this machine on.`

// runMCP runs dbgp as an MCP server on stdin and stdout until the client
// disconnects. stdout carries the protocol, so diagnostics go to stderr.
func runMCP(args []string) int {
	fs := flag.NewFlagSet("dbgp mcp", flag.ContinueOnError)
	addr := fs.String("addr", dbgp.DefaultAddr, "address to listen on for PHP connections")
	idekey := fs.String("idekey", "", "accept only sessions with this IDE key")
	listen := fs.Bool("listen", false, "listen for PHP connections from the start, instead of waiting for the listen tool")
	var maps listFlag
	fs.Var(&maps, "map", "path mapping `LOCAL=REMOTE` for PHP in a container or on a server (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: dbgp mcp [options]\n\n"+
			"Runs dbgp as an MCP server on stdin and stdout, for AI agents.\n"+
			"It does not listen for PHP connections until asked to.\n\nOptions:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	pathMap, err := parsePathMap(maps)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	srv, err := dbgp.NewServer(dbgp.Config{Addr: *addr, IDEKey: *idekey, PathMap: pathMap})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer srv.Close()
	if *listen {
		if err := srv.StartListening(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newMCPServer(srv).Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// newMCPServer returns an MCP server whose tools drive srv.
func newMCPServer(srv *dbgp.Server) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "dbgp", Title: "PHP debugger", Version: version},
		&mcp.ServerOptions{Instructions: mcpInstructions})
	t := &tools{srv: srv}

	mcp.AddTool(server, &mcp.Tool{
		Name: "status",
		Description: "Show whether dbgp is listening for PHP connections and on which address, " +
			"the debugging sessions (one per PHP request or script run), breakpoints and path mappings.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		OutputSchema: outputSchema[statusOutput]("sessions", "breakpoints", "pathMappings"),
	}, t.status)
	mcp.AddTool(server, &mcp.Tool{
		Name: "listen",
		Description: "Start listening for PHP connections, so the next PHP script or request with " +
			"debugging enabled connects and becomes a session. The result gives the address PHP must connect to.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, t.listen)
	mcp.AddTool(server, &mcp.Tool{
		Name: "unlisten",
		Description: "Stop listening: new PHP connections are refused, so PHP runs at full speed. " +
			"Sessions already connected are not affected.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, t.unlisten)
	return server
}

// outputSchema returns the schema for a tool's output type, with the named
// list properties declared as always a list: the schema inferred from a Go
// slice also allows null, but the tools always send a list, empty or not.
func outputSchema[T any](lists ...string) *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("output schema: %v", err))
	}
	for _, name := range lists {
		prop := schema.Properties[name]
		prop.Types, prop.Type = nil, "array"
	}
	return schema
}

// tools implements the MCP tools on a dbgp server.
type tools struct {
	srv *dbgp.Server
}

type noInput struct{}

type statusOutput struct {
	Listening    bool             `json:"listening" jsonschema:"whether PHP connections are accepted"`
	Address      string           `json:"address" jsonschema:"the address PHP connects to when listening"`
	Sessions     []sessionInfo    `json:"sessions" jsonschema:"debugging sessions, oldest first, including ended ones"`
	Breakpoints  []breakpointInfo `json:"breakpoints" jsonschema:"breakpoints, applied to every session"`
	PathMappings []pathMapping    `json:"pathMappings" jsonschema:"local directories mapped to the directories PHP sees"`
}

type sessionInfo struct {
	ID     int    `json:"id" jsonschema:"session id, used by the tools that act on a session"`
	Script string `json:"script" jsonschema:"local path of the script being debugged"`
	Status string `json:"status" jsonschema:"starting, running, break (stopped), stopping (script finished) or ended"`
	File   string `json:"file,omitempty" jsonschema:"local path of the file where execution is stopped, when status is break"`
	Line   int    `json:"line,omitempty" jsonschema:"line where execution is stopped, when status is break"`
}

type breakpointInfo struct {
	ID        int    `json:"id" jsonschema:"breakpoint id, used to remove it"`
	Type      string `json:"type" jsonschema:"line, conditional, call, return or exception"`
	File      string `json:"file,omitempty" jsonschema:"local path of the file, for line and conditional breakpoints"`
	Line      int    `json:"line,omitempty" jsonschema:"line number, for line and conditional breakpoints"`
	Condition string `json:"condition,omitempty" jsonschema:"PHP expression that must be true to stop, for conditional breakpoints"`
	Function  string `json:"function,omitempty" jsonschema:"function or method name, for call and return breakpoints"`
	Exception string `json:"exception,omitempty" jsonschema:"exception class (subclasses also stop), for exception breakpoints"`
}

type pathMapping struct {
	Local  string `json:"local" jsonschema:"the directory on this machine"`
	Remote string `json:"remote" jsonschema:"the directory as PHP sees it"`
}

func (t *tools) status(context.Context, *mcp.CallToolRequest, noInput) (*mcp.CallToolResult, statusOutput, error) {
	out := statusOutput{
		Listening:    t.srv.Listening(),
		Address:      t.srv.Addr(),
		Sessions:     []sessionInfo{},
		Breakpoints:  []breakpointInfo{},
		PathMappings: []pathMapping{},
	}
	for _, s := range t.srv.Sessions() {
		st := s.State()
		info := sessionInfo{ID: s.ID(), Script: s.Script(), Status: st.Status}
		if ended(st) {
			info.Status = "ended"
		} else if st.Status == dbgp.StatusBreak {
			info.File, info.Line = st.File, st.Line
		}
		out.Sessions = append(out.Sessions, info)
	}
	for _, bp := range t.srv.Breakpoints() {
		out.Breakpoints = append(out.Breakpoints, breakpointInfo{
			ID: bp.ID, Type: bp.Type, File: bp.File, Line: bp.Line,
			Condition: bp.Condition, Function: bp.Function, Exception: bp.Exception,
		})
	}
	for _, m := range t.srv.PathMap() {
		out.PathMappings = append(out.PathMappings, pathMapping{Local: m.Local, Remote: m.Remote})
	}
	return nil, out, nil
}

type listenOutput struct {
	Listening bool   `json:"listening" jsonschema:"whether PHP connections are accepted"`
	Address   string `json:"address" jsonschema:"the address PHP must connect to (host and port)"`
}

func (t *tools) listen(context.Context, *mcp.CallToolRequest, noInput) (*mcp.CallToolResult, listenOutput, error) {
	if err := t.srv.StartListening(); err != nil {
		return nil, listenOutput{}, err
	}
	return nil, listenOutput{Listening: true, Address: t.srv.Addr()}, nil
}

type unlistenOutput struct {
	Listening bool `json:"listening" jsonschema:"whether PHP connections are accepted"`
}

func (t *tools) unlisten(context.Context, *mcp.CallToolRequest, noInput) (*mcp.CallToolResult, unlistenOutput, error) {
	if err := t.srv.StopListening(); err != nil {
		return nil, unlistenOutput{Listening: t.srv.Listening()}, err
	}
	return nil, unlistenOutput{Listening: false}, nil
}
