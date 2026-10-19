package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	dbgp "github.com/php-debugger/dbgp-client"
)

// claimTimeout is how long a session waits for an agent to take it before
// it is detached: PHP Debugger debugs every request, so while dbgp listens,
// requests nobody debugs would otherwise hold their PHP workers.
const claimTimeout = time.Minute

// mcpInstructions are sent to MCP clients when they connect.
const mcpInstructions = `dbgp debugs PHP scripts and requests through PHP Debugger or Xdebug.
It does not listen for PHP connections until the listen tool is called, so
PHP runs at full speed until you want to debug. Call status to see what is
going on, listen before starting the PHP code to debug, and unlisten when done.
Remove each breakpoint with remove_breakpoint as soon as you no longer need it:
breakpoints apply to every later session and slow it down.

A typical run: add_breakpoint, listen, start the PHP code, wait_for_session,
then continue to run to the breakpoint. Start PHP in the background, or send
the web request without waiting for its answer: PHP waits for dbgp at every
stop, so it does not finish while you are debugging it. Once stopped, look
around with stack, variables, variable and source, and step or run on with
continue. output and warnings show what the script printed and the PHP
warnings it raised. End every session with stop or detach when you are done
with it: until then its PHP process or web server worker is held. A session
that neither wait_for_session nor a tool naming it by id takes within a minute
of connecting is detached, so its script runs on without the debugger: call
wait_for_session right after starting the PHP code.

Reading an object property that does not exist, with variable or
variable_value, makes PHP call the object's __isset and __get methods: the
script's own code runs, and may change its state or have other side effects.
Listing variables and reading properties that exist run no code, so take
property names from what variables and variable list, and do not guess them.

The session tools act on the session wait_for_session last returned, or the
last one you named by id. Each session shows the address PHP connected from:
127.0.0.1 or ::1 for PHP on this machine. dbgp accepts PHP connections from
every network this machine is on, so stop any session that is not from where
your PHP runs, and do not trust what it shows.

Long results come in parts: lists in pages, see page and pages, and long text
when more or next is set. Ask only for the parts you need.

Before starting PHP, check which debugger it has with php -v: it prints
"with PHP Debugger" or "with Xdebug". PHP Debugger is always in debug mode and
starts debugging every request by itself, so do not set xdebug.mode or
xdebug.start_with_request for it. Xdebug needs xdebug.mode=debug and
xdebug.start_with_request=yes.

PHP finds dbgp through xdebug.client_host and xdebug.client_port, which
default to localhost and 9003 in both debuggers. dbgp listens on every address
of this machine, on the port the listen tool returns, 9003 by default. When PHP
runs on this machine and the port is 9003, those defaults already reach dbgp
and neither needs to be set. Otherwise set client_port to that port, and
client_host to an address PHP can reach this machine on, such as
host.docker.internal from a Docker container.`

// noCodeInstructions are added to mcpInstructions when dbgp mcp runs with
// -no-eval, so agents know the code tools were left out on purpose.
const noCodeInstructions = `

The user started dbgp with -no-eval, which turns off running PHP code you
write in the debugged script: the eval and set_variable tools, and conditions on
breakpoints. So you cannot evaluate PHP, change variables or set conditional
breakpoints. This is the user's choice, not a missing feature or an outdated
build: do not suggest rebuilding or restarting dbgp to get them. Read values
with variables, variable and variable_value instead, and work out results from
them. Instead of a condition, stop at the line and check the values each time.`

// sharedInstructions are added to mcpInstructions on the background server,
// which agents share.
const sharedInstructions = `

This is the user's background dbgp, which other agents may be using at the
same time. They share its breakpoints, path mappings and sessions; each has its
own selected session, and its own place in output and warnings. Remove only the
breakpoints you added. dbgp keeps listening until every agent that called
listen has called unlisten, so unlisten when you are done.
wait_for_session returns the oldest session no agent has taken yet, which may
be another agent's: if its script is not yours, leave it alone and wait again.
If the wait ends although your PHP started, another agent may have taken your
session: find it with sessions and name it by id.`

// runMCP runs dbgp as an MCP server. Agents start it as dbgp mcp and talk
// to it on stdin and stdout: it serves them itself, or passes them on to the
// background server when one is running, so they share it. dbgp mcp start,
// stop and status manage the background server, which serves MCP over HTTP.
func runMCP(args []string) int {
	command := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	switch command {
	case "", "start", serveCommand:
	case "stop", "status":
		if len(args) > 0 {
			fmt.Fprintf(os.Stderr, "dbgp mcp %s takes no options\n", command)
			return 2
		}
		if command == "stop" {
			return stopDaemon()
		}
		return daemonStatus()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q: use dbgp mcp, dbgp mcp start, stop or status\n", command)
		return 2
	}

	fs := flag.NewFlagSet("dbgp mcp", flag.ContinueOnError)
	phpPort := fs.Int("php-port", defaultPHPPort, "port to listen on for PHP connections, on every address of this machine")
	idekey := fs.String("idekey", "", "accept only sessions with this IDE key")
	listen := fs.Bool("listen", false, "listen for PHP connections from the start, instead of waiting for the listen tool")
	noEval := fs.Bool("no-eval", false, "turn off running PHP code the agent writes in the debugged script: the eval and set_variable tools, and breakpoint conditions")
	mcpPort := fs.Int("mcp-port", defaultHTTPPort, "with start: the port agents connect to over HTTP, on "+httpHost+" only")
	var maps listFlag
	fs.Var(&maps, "map", "path mapping `LOCAL=REMOTE` for PHP in a container or on a server (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: dbgp mcp [options]\n       dbgp mcp start [options]\n       dbgp mcp stop\n       dbgp mcp status\n\n"+
			"dbgp mcp runs dbgp as an MCP server for AI agents, on stdin and stdout:\n"+
			"agents start it themselves. When the background server is running, it\n"+
			"connects the agent to that instead, and the background server's options apply.\n"+
			"dbgp mcp start starts the background server, which agents share, and returns;\n"+
			"dbgp mcp stop stops it and dbgp mcp status shows whether it is running.\n"+
			"dbgp does not listen for PHP connections until asked to.\n\nOptions:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	pathMap, err := parsePathMap(maps)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for name, port := range map[string]int{"-php-port": *phpPort, "-mcp-port": *mcpPort} {
		if err := checkPort(name, port); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch command {
	case "start":
		return startDaemon(args)
	case "":
		st, err := running()
		if err != nil {
			// Serve the agent here instead.
			fmt.Fprintln(os.Stderr, "dbgp:", err)
		}
		if st != nil {
			if len(args) > 0 {
				fmt.Fprintf(os.Stderr, "dbgp: using the background server at %s, with its own options: %s ignored\n",
					st.URL, strings.Join(args, " "))
			}
			if err := bridge(ctx, st); err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			return 0
		}
	}

	srv, err := dbgp.NewServer(dbgp.Config{
		Addr: phpAddr(*phpPort), IDEKey: *idekey, PathMap: pathMap, ClaimTimeout: claimTimeout,
		// The tools never read the engine mapping that the probe reports.
		NoEval: *noEval,
	})
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
	server, t := newMCPServer(srv, !*noEval, command == serveCommand)
	if command == serveCommand {
		err = serveHTTP(ctx, server, t, *mcpPort)
	} else {
		err = server.Run(ctx, &mcp.StdioTransport{})
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// newMCPServer returns an MCP server whose tools drive srv. allowCode adds
// what runs the agent's PHP code in the debugged script: the code tools, and
// conditions on breakpoints. shared is for the background server, which agents share.
// The tools are returned too, for what they keep about agents.
func newMCPServer(srv *dbgp.Server, allowCode, shared bool) (*mcp.Server, *tools) {
	instructions := mcpInstructions
	if !allowCode {
		instructions += noCodeInstructions
	}
	if shared {
		instructions += sharedInstructions
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "dbgp", Title: "PHP debugger", Version: version},
		&mcp.ServerOptions{Instructions: instructions})
	t := &tools{srv: srv, allowCode: allowCode}

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
			"debugging enabled connects and becomes a session. The result gives the port PHP must connect to.",
		Annotations:  &mcp.ToolAnnotations{IdempotentHint: true},
		OutputSchema: outputSchema[listenOutput]("warnings"),
	}, t.listen)
	mcp.AddTool(server, &mcp.Tool{
		Name: "unlisten",
		Description: "Stop listening: new PHP connections are refused, so PHP runs at full speed. " +
			"Sessions already connected are not affected. On the background server, dbgp keeps listening " +
			"while other agents that called listen have not called unlisten.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, t.unlisten)

	notDestructive := false
	conditions := "A line breakpoint with a condition stops only when the condition is true. "
	if !allowCode {
		conditions = "Conditions are turned off (-no-eval), since they run PHP code. "
	}
	mcp.AddTool(server, &mcp.Tool{
		Name: "add_breakpoint",
		Description: "Add a breakpoint. It applies to every debugging session, including ones that connect later. " +
			conditions +
			"Remove it with remove_breakpoint once you no longer need it: breakpoints slow down later sessions.",
		Annotations:  &mcp.ToolAnnotations{DestructiveHint: &notDestructive},
		InputSchema:  addBreakpointSchema(allowCode),
		OutputSchema: outputSchema[addBreakpointOutput](),
	}, t.addBreakpoint)
	mcp.AddTool(server, &mcp.Tool{
		Name:         "remove_breakpoint",
		Description:  "Remove a breakpoint by id, from every session. Returns the remaining breakpoints.",
		OutputSchema: outputSchema[breakpointsOutput]("breakpoints"),
	}, t.removeBreakpoint)
	mcp.AddTool(server, &mcp.Tool{
		Name:         "breakpoints",
		Description:  "List the breakpoints.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		OutputSchema: outputSchema[breakpointListOutput]("breakpoints"),
	}, t.breakpoints)
	mcp.AddTool(server, &mcp.Tool{
		Name: "add_path_mapping",
		Description: "Map a directory on this machine to the directory PHP sees it as, for PHP running in a " +
			"container or on another machine. Paths passed to and returned by dbgp are then paths on this machine. " +
			"A mapping for a local directory that is already mapped replaces it. Applies to connected sessions at once.",
		Annotations:  &mcp.ToolAnnotations{DestructiveHint: &notDestructive, IdempotentHint: true},
		OutputSchema: outputSchema[pathMappingsOutput]("pathMappings"),
	}, t.addPathMapping)
	mcp.AddTool(server, &mcp.Tool{
		Name:         "path_mappings",
		Description:  "List the path mappings.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		OutputSchema: outputSchema[pathMappingListOutput]("pathMappings"),
	}, t.pathMappings)
	t.addSessionTools(server)
	t.addInspectTools(server)
	t.addOutputTools(server)
	if allowCode {
		t.addCodeTools(server)
	}
	return server, t
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

// Tool results are kept small enough for an agent's context: longer text
// and lists are returned in parts, which the agent asks for one at a time.
const (
	maxTextBytes = 32 << 10 // output, and values from variable_value
	maxLineBytes = 500      // each line from source
	maxListItems = 50       // stack frames, variables and warnings
)

// cutText returns at most limit bytes of s, cut at a character boundary.
func cutText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// pageOf returns page n of items, maxListItems per page, and how many pages
// there are; there is always at least one, maybe empty.
func pageOf[T any](items []T, n int) ([]T, int, error) {
	pages := max(1, (len(items)+maxListItems-1)/maxListItems)
	if n < 0 || n >= pages {
		return nil, pages, fmt.Errorf("page must be from 0 to %d", pages-1)
	}
	return items[n*maxListItems : min(len(items), (n+1)*maxListItems)], pages, nil
}

// tools implements the MCP tools on a dbgp server.
type tools struct {
	srv *dbgp.Server
	// allowCode allows what runs the agent's PHP code in the debugged script.
	allowCode bool

	// mu guards agents, by agent id (see agentID), and detaching.
	mu     sync.Mutex
	agents map[string]*agentState
	// detaching holds the ids of the sessions being detached because the
	// agents that took them left: no agent can take them meanwhile.
	detaching map[int]bool
}

// agentState is what the tools keep for one agent. Agents sharing the
// background server each have their own, so one does not change what the
// tools do for another.
type agentState struct {
	// selected is the session the tools act on by default: the one
	// wait_for_session last returned or a tool was last given by id. Not
	// simply the newest, which anything that connects would become.
	selected int
	// taken holds the ids of the sessions the agent has taken, with
	// wait_for_session or by naming them, to detach when it leaves.
	taken map[int]bool
	// output and warnings return only what is new since their last call,
	// so they keep read positions per session id.
	outputFrom, warningsFrom map[int]int
	// listening is set from the agent's listen to its unlisten. dbgp keeps
	// listening while any agent has it set, so one agent does not stop
	// PHP connections another is waiting for.
	listening bool
}

// agent returns the state of the agent that sent req. t.mu must be held.
func (t *tools) agent(req *mcp.CallToolRequest) *agentState {
	id := agentID(req)
	a := t.agents[id]
	if a == nil {
		if t.agents == nil {
			t.agents = map[string]*agentState{}
		}
		a = &agentState{taken: map[int]bool{}, outputFrom: map[int]int{}, warningsFrom: map[int]int{}}
		t.agents[id] = a
	}
	return a
}

// take makes session id a's selected session, and one it has taken.
func (a *agentState) take(id int) {
	a.selected = id
	a.taken[id] = true
}

// leave forgets the agent with the given id, which has gone: it no longer
// keeps dbgp listening, and the sessions it took are detached, so their
// scripts run on instead of holding their PHP workers, unless another agent
// has taken them too.
func (t *tools) leave(id string) error {
	orphans, err := t.release(id)
	t.detachSessions(orphans)
	return err
}

// release forgets the agent with the given id, stops listening if it was
// the last agent listening, and returns the live sessions only it took,
// marked as detaching, so no other agent takes one before it is detached.
func (t *tools) release(id string) ([]*dbgp.Session, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.agents[id]
	if a == nil {
		return nil, nil
	}
	delete(t.agents, id)
	var orphans []*dbgp.Session
	for sid := range a.taken {
		if t.taken(sid) {
			continue
		}
		if s := t.srv.Session(sid); s != nil && !ended(s.State()) {
			if t.detaching == nil {
				t.detaching = map[int]bool{}
			}
			t.detaching[sid] = true
			orphans = append(orphans, s)
		}
	}
	if a.listening && t.listeners() == 0 {
		return orphans, t.srv.StopListening()
	}
	return orphans, nil
}

// detachSessions detaches the sessions release returned, outside t.mu:
// detaching a stopped session waits for PHP's answer. Once they have ended,
// agents can name them again, to read their output.
func (t *tools) detachSessions(sessions []*dbgp.Session) {
	for _, s := range sessions {
		_ = s.Detach()
		_ = s.Close()
		<-s.Done()
		t.mu.Lock()
		delete(t.detaching, s.ID())
		t.mu.Unlock()
	}
}

// taken reports whether any agent has taken session id. t.mu must be held.
func (t *tools) taken(id int) bool {
	for _, a := range t.agents {
		if a.taken[id] {
			return true
		}
	}
	return false
}

// listeners returns how many agents have called listen and not unlisten.
// t.mu must be held.
func (t *tools) listeners() int {
	n := 0
	for _, a := range t.agents {
		if a.listening {
			n++
		}
	}
	return n
}

// agentID identifies the agent that sent req: on the background server, by
// the agentHeader that its dbgp mcp sends. An agent on stdin and stdout is
// the only one, with no id.
func agentID(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil {
		return ""
	}
	return req.Extra.Header.Get(agentHeader)
}

type noInput struct{}

type statusOutput struct {
	Listening    bool             `json:"listening" jsonschema:"whether PHP connections are accepted"`
	Port         int              `json:"port" jsonschema:"the port PHP connects to when listening, on any address of this machine"`
	Sessions     []sessionInfo    `json:"sessions" jsonschema:"debugging sessions, oldest first, including ended ones"`
	Breakpoints  []breakpointInfo `json:"breakpoints" jsonschema:"breakpoints, applied to every session"`
	PathMappings []pathMapping    `json:"pathMappings" jsonschema:"local directories mapped to the directories PHP sees"`
}

type sessionInfo struct {
	ID     int    `json:"id" jsonschema:"session id, used by the tools that act on a session"`
	From   string `json:"from" jsonschema:"the address PHP connected from: 127.0.0.1 or ::1 for PHP on this machine"`
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
		Listening: t.srv.Listening(),
		Port:      t.srv.Port(),
		Sessions:  t.sessionList(),
	}
	out.Breakpoints = t.breakpointList()
	out.PathMappings = t.pathMappingList()
	return nil, out, nil
}

// breakpointList returns the server's breakpoints, never nil.
func (t *tools) breakpointList() []breakpointInfo {
	list := []breakpointInfo{}
	for _, bp := range t.srv.Breakpoints() {
		list = append(list, toBreakpointInfo(bp))
	}
	return list
}

func toBreakpointInfo(bp dbgp.Breakpoint) breakpointInfo {
	return breakpointInfo{
		ID: bp.ID, Type: bp.Type, File: bp.File, Line: bp.Line,
		Condition: bp.Condition, Function: bp.Function, Exception: bp.Exception,
	}
}

// pathMappingList returns the server's path mappings, never nil.
func (t *tools) pathMappingList() []pathMapping {
	list := []pathMapping{}
	for _, m := range t.srv.PathMap() {
		list = append(list, pathMapping{Local: m.Local, Remote: m.Remote})
	}
	return list
}

type listenOutput struct {
	Listening bool     `json:"listening" jsonschema:"whether PHP connections are accepted"`
	Port      int      `json:"port" jsonschema:"the port PHP must connect to (xdebug.client_port), on any address of this machine"`
	Warnings  []string `json:"warnings,omitempty" jsonschema:"addresses dbgp could not listen on, although it listens on others; PHP connecting there may reach another program instead"`
}

func (t *tools) listen(_ context.Context, req *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, listenOutput, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.srv.StartListening(); err != nil {
		return nil, listenOutput{}, err
	}
	t.agent(req).listening = true
	return nil, listenOutput{Listening: true, Port: t.srv.Port(), Warnings: t.srv.ListenWarnings()}, nil
}

type unlistenOutput struct {
	Listening   bool `json:"listening" jsonschema:"whether PHP connections are accepted"`
	OtherAgents int  `json:"otherAgents,omitempty" jsonschema:"how many other agents called listen and not unlisten yet: dbgp keeps listening for them"`
}

func (t *tools) unlisten(_ context.Context, req *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, unlistenOutput, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.agent(req).listening = false
	if others := t.listeners(); others > 0 {
		return nil, unlistenOutput{Listening: t.srv.Listening(), OtherAgents: others}, nil
	}
	if err := t.srv.StopListening(); err != nil {
		return nil, unlistenOutput{Listening: t.srv.Listening()}, err
	}
	return nil, unlistenOutput{Listening: false}, nil
}

// breakpointTypes are the breakpoint types add_breakpoint accepts; a line
// breakpoint with a condition becomes a conditional one.
var breakpointTypes = []any{dbgp.BreakpointLine, dbgp.BreakpointCall, dbgp.BreakpointReturn, dbgp.BreakpointException}

type addBreakpointInput struct {
	Type string `json:"type,omitempty" jsonschema:"line (the default) stops at a file and line; call and return stop when a function is called or returns; exception stops when an exception, or a subclass of it, is thrown"`
	File string `json:"file,omitempty" jsonschema:"for line: the PHP file, as an absolute path on this machine"`
	Line int    `json:"line,omitempty" jsonschema:"for line: the line number"`
	// A condition makes a line breakpoint a conditional one.
	Condition string `json:"condition,omitempty" jsonschema:"for line, optional: a PHP expression; execution stops only when it is true"`
	Function  string `json:"function,omitempty" jsonschema:"for call and return: the function name"`
	Exception string `json:"exception,omitempty" jsonschema:"for exception: the exception class"`
}

// addBreakpointSchema is addBreakpointInput's schema, listing the types.
// Without allowCode it has no condition, which is PHP code.
func addBreakpointSchema(allowCode bool) *jsonschema.Schema {
	schema, err := jsonschema.For[addBreakpointInput](nil)
	if err != nil {
		panic(fmt.Sprintf("add_breakpoint schema: %v", err))
	}
	schema.Properties["type"].Enum = breakpointTypes
	if !allowCode {
		delete(schema.Properties, "condition")
		schema.PropertyOrder = slices.DeleteFunc(schema.PropertyOrder, func(name string) bool { return name == "condition" })
	}
	return schema
}

type addBreakpointOutput struct {
	Breakpoint breakpointInfo `json:"breakpoint" jsonschema:"the breakpoint added"`
	Warning    string         `json:"warning,omitempty" jsonschema:"set when the breakpoint was added but a connected session rejected it"`
}

func (t *tools) addBreakpoint(_ context.Context, _ *mcp.CallToolRequest, in addBreakpointInput) (*mcp.CallToolResult, addBreakpointOutput, error) {
	if in.Condition != "" && !t.allowCode {
		return nil, addBreakpointOutput{}, errors.New("conditions are turned off (-no-eval), since they run PHP code: " +
			"add the breakpoint without one and check the values each time it stops")
	}
	bp := dbgp.Breakpoint{Type: in.Type, Line: in.Line, Function: in.Function, Exception: in.Exception}
	if bp.Type == "" {
		bp.Type = dbgp.BreakpointLine
	}
	if bp.Type == dbgp.BreakpointLine && in.Condition != "" {
		bp.Type, bp.Condition = dbgp.BreakpointConditional, in.Condition
	}
	if in.File != "" {
		file, err := filePath("file", in.File)
		if err != nil {
			return nil, addBreakpointOutput{}, err
		}
		bp.File = file
	}
	added, err := t.srv.AddBreakpoint(bp)
	if added.ID == 0 {
		return nil, addBreakpointOutput{}, err
	}
	out := addBreakpointOutput{Breakpoint: toBreakpointInfo(added)}
	if err != nil {
		out.Warning = err.Error()
	}
	return nil, out, nil
}

// filePath is absolutePath for a file, which can also be given as a
// file:// URI, kept as it is.
func filePath(name, path string) (string, error) {
	if strings.HasPrefix(path, "file://") {
		return path, nil
	}
	return absolutePath(name, path)
}

// absolutePath checks that the path given as the named argument is
// absolute, and cleans it. Relative paths are refused rather than taken as
// relative to dbgp's working directory, which is not the agent's: the
// background server's is wherever dbgp mcp start ran.
func absolutePath(name, path string) (string, error) {
	// On Windows, a path from the root such as \app\index.php is not
	// absolute, but means the current drive; Abs adds it.
	if !filepath.IsAbs(path) && (path == "" || !os.IsPathSeparator(path[0])) {
		return "", fmt.Errorf("%s must be an absolute path, not %q", name, path)
	}
	return filepath.Abs(path)
}

type breakpointIDInput struct {
	ID int `json:"id" jsonschema:"the breakpoint id, from add_breakpoint, breakpoints or status"`
}

type breakpointsOutput struct {
	Breakpoints []breakpointInfo `json:"breakpoints" jsonschema:"the breakpoints, applied to every session"`
	Warning     string           `json:"warning,omitempty" jsonschema:"set when a connected session could not remove the breakpoint"`
}

func (t *tools) removeBreakpoint(_ context.Context, _ *mcp.CallToolRequest, in breakpointIDInput) (*mcp.CallToolResult, breakpointsOutput, error) {
	exists := false
	for _, bp := range t.srv.Breakpoints() {
		exists = exists || bp.ID == in.ID
	}
	if !exists {
		return nil, breakpointsOutput{}, fmt.Errorf("no breakpoint %d", in.ID)
	}
	// It exists, so an error means a session could not remove it.
	err := t.srv.RemoveBreakpoint(in.ID)
	out := breakpointsOutput{Breakpoints: t.breakpointList()}
	if err != nil {
		out.Warning = err.Error()
	}
	return nil, out, nil
}

type breakpointListOutput struct {
	Breakpoints []breakpointInfo `json:"breakpoints" jsonschema:"the breakpoints, applied to every session"`
}

func (t *tools) breakpoints(context.Context, *mcp.CallToolRequest, noInput) (*mcp.CallToolResult, breakpointListOutput, error) {
	return nil, breakpointListOutput{Breakpoints: t.breakpointList()}, nil
}

type addPathMappingInput struct {
	Local  string `json:"local" jsonschema:"the directory on this machine, as an absolute path"`
	Remote string `json:"remote" jsonschema:"the same directory as PHP sees it, e.g. /var/www/html in a container"`
}

type pathMappingsOutput struct {
	PathMappings []pathMapping `json:"pathMappings" jsonschema:"local directories mapped to the directories PHP sees"`
	Warning      string        `json:"warning,omitempty" jsonschema:"set when the mapping was added but a connected session rejected a breakpoint set again with it"`
}

func (t *tools) addPathMapping(_ context.Context, _ *mcp.CallToolRequest, in addPathMappingInput) (*mcp.CallToolResult, pathMappingsOutput, error) {
	if in.Local == "" || in.Remote == "" {
		return nil, pathMappingsOutput{}, errors.New("local and remote must both be set")
	}
	local, err := absolutePath("local", in.Local)
	if err != nil {
		return nil, pathMappingsOutput{}, err
	}
	// Both are set, so an error means a session rejected a breakpoint
	// set again with the new path.
	err = t.srv.AddPathMapping(dbgp.PathMapping{Local: local, Remote: in.Remote})
	out := pathMappingsOutput{PathMappings: t.pathMappingList()}
	if err != nil {
		out.Warning = err.Error()
	}
	return nil, out, nil
}

type pathMappingListOutput struct {
	PathMappings []pathMapping `json:"pathMappings" jsonschema:"local directories mapped to the directories PHP sees"`
}

func (t *tools) pathMappings(context.Context, *mcp.CallToolRequest, noInput) (*mcp.CallToolResult, pathMappingListOutput, error) {
	return nil, pathMappingListOutput{PathMappings: t.pathMappingList()}, nil
}
