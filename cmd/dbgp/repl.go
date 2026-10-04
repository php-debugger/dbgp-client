package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dbgp "github.com/php-debugger/dbgp-client"
)

// sourceContext is how many lines source shows around a location.
const sourceContext = 4

// forever is how long run and step commands wait for the script to stop;
// Ctrl-C ends the wait.
const forever = time.Duration(math.MaxInt64)

// repl runs debugger commands against a server's sessions.
type repl struct {
	srv *dbgp.Server

	// onPrompt, if set, is told the new prompt when it changes while a line
	// is being read (a session connected).
	onPrompt func(string)

	mu         sync.Mutex // guards the fields below and writes to out
	out        io.Writer
	cur        *dbgp.Session
	cancel     context.CancelFunc // cancels the running command
	outputFrom map[int]int        // per session: Output offset already shown
	notesFrom  map[int]int        // per session: notifications already shown
}

func newREPL(srv *dbgp.Server, out io.Writer) *repl {
	return &repl{srv: srv, out: out, outputFrom: map[int]int{}, notesFrom: map[int]int{}}
}

func (r *repl) printf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, format, args...)
}

// run reads commands until quit or end of input.
func (r *repl) run(lines lineReader) {
	for {
		line, err := lines.ReadLine(r.prompt())
		if isInterrupt(err) {
			// Ctrl-C at the prompt discards the line being typed.
			r.printf("(type quit to exit)\n")
			continue
		}
		if err != nil {
			return
		}
		if r.execLine(line) {
			return
		}
	}
}

func (r *repl) prompt() string {
	s := r.session()
	if s == nil {
		return "(dbgp) "
	}
	st := s.State()
	if ended(st) {
		return fmt.Sprintf("(dbgp %d:ended) ", s.ID())
	}
	return fmt.Sprintf("(dbgp %d:%s) ", s.ID(), st.Status)
}

func (r *repl) session() *dbgp.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

// announceSessions reports new sessions and selects the first one.
func (r *repl) announceSessions() {
	for {
		s, err := r.srv.WaitForSession(context.Background())
		if err != nil {
			return
		}
		r.mu.Lock()
		selected := r.cur == nil || ended(r.cur.State())
		if selected {
			r.cur = s
		}
		r.mu.Unlock()
		note := ""
		if selected {
			note = " (selected)"
			if r.onPrompt != nil {
				r.onPrompt(r.prompt())
			}
		}
		r.printf("\n[session %d connected: %s%s]\n", s.ID(), s.Script(), note)
		for _, w := range s.SetupWarnings() {
			r.printf("[session %d warning: %s]\n", s.ID(), w)
		}
	}
}

// cancelOnInterrupt cancels the running command on Ctrl-C.
func (r *repl) cancelOnInterrupt(interrupts <-chan os.Signal) {
	for range interrupts {
		r.mu.Lock()
		cancel := r.cancel
		r.mu.Unlock()
		if cancel != nil {
			cancel()
		} else {
			r.printf("\n(type quit to exit)\n%s", r.prompt())
		}
	}
}

// execLine runs one command line and reports whether to quit.
func (r *repl) execLine(line string) (quit bool) {
	name, args, _ := strings.Cut(strings.TrimSpace(line), " ")
	args = strings.TrimSpace(args)
	if name == "" {
		return false
	}
	if name == "quit" || aliases[name] == "quit" {
		return true
	}
	cmd, ok := commands[name]
	if !ok {
		r.printf("Unknown command %q. Type help for commands.\n", name)
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.cancel = nil
		r.mu.Unlock()
		cancel()
	}()

	if err := cmd.run(r, ctx, args); err != nil {
		if errors.Is(err, context.Canceled) {
			r.printf("Interrupted.\n")
		} else {
			r.printf("Error: %v\n", err)
		}
	}
	return false
}

type command struct {
	usage string // one line per form
	help  string // one line per form; later forms may have none
	run   func(r *repl, ctx context.Context, args string) error
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"help":     {"help", "show this list", (*repl).cmdHelp},
		"sessions": {"sessions", "list sessions, and whether PHP can connect", (*repl).cmdSessions},
		"listen":   {"listen", "accept PHP connections (the default when dbgp starts)", (*repl).cmdListen},
		"map":      {"map LOCAL=REMOTE", "map a local directory to the one PHP sees, e.g. in a container", (*repl).cmdMap},
		"maps":     {"maps", "list path mappings", (*repl).cmdMaps},
		"unlisten": {"unlisten", "refuse new PHP connections, so scripts run undebugged", (*repl).cmdUnlisten},
		"use":      {"use ID", "select a session", (*repl).cmdUse},
		"break": {"break [FILE:]LINE [if COND]\nbreak call|return FUNC\nbreak exception CLASS",
			"stop at a line (in the current file if FILE is left out), or only when COND is true\n" +
				"stop when the function or method FUNC is called, or when it returns\n" +
				"stop when an exception of CLASS, or a subclass, is thrown",
			(*repl).cmdBreak},
		"breaks":   {"breaks", "list breakpoints", (*repl).cmdBreaks},
		"delete":   {"delete ID", "remove a breakpoint", (*repl).cmdDelete},
		"run":      {"run", "run to the next breakpoint", continueWith(dbgp.ContinueRun)},
		"step":     {"step", "step into the next statement", continueWith(dbgp.ContinueStepInto)},
		"next":     {"next", "step over the next statement", continueWith(dbgp.ContinueStepOver)},
		"out":      {"out", "step out of the current function", continueWith(dbgp.ContinueStepOut)},
		"stack":    {"stack", "show the call stack", (*repl).cmdStack},
		"vars":     {"vars [DEPTH [CONTEXT]]", "list the variables of a stack frame (see contexts); print shows inside them", (*repl).cmdVars},
		"contexts": {"contexts", "list variable contexts", (*repl).cmdContexts},
		"print":    {"print [-p PAGE] [-d DEPTH] NAME", "show a variable and its children, e.g. print $user[\"name\"]", (*repl).cmdPrint},
		"value":    {"value [-d DEPTH] NAME", "show a variable's whole value", (*repl).cmdValue},
		"eval":     {"eval CODE", "evaluate PHP code", (*repl).cmdEval},
		"set":      {"set NAME = EXPR", "assign a PHP expression to a variable", (*repl).cmdSet},
		"source":   {"source [[FILE:]LINE]", "show source around a location", (*repl).cmdSource},
		"output":   {"output", "show new program output", (*repl).cmdOutput},
		"notes":    {"notes", "show new warnings and other notifications", (*repl).cmdNotes},
		"stop":     {"stop", "end the script", (*repl).cmdStop},
		"detach":   {"detach", "let the script run on without the debugger", (*repl).cmdDetach},
		// Handled by execLine; listed here for help and completion.
		"quit": {"quit", "exit the debugger", nil},
	}
	for alias, name := range aliases {
		commands[alias] = commands[name]
	}
}

// usageError reports a command's usage on one line.
func usageError(name string) error {
	return errors.New("usage: " + strings.ReplaceAll(commands[name].usage, "\n", " | "))
}

// aliases are short names for common commands. bt (backtrace) and
// exit follow gdb.
var aliases = map[string]string{
	"r": "run", "s": "step", "n": "next", "o": "out", "bt": "stack", "p": "print",
	"b": "break", "l": "source", "q": "quit", "exit": "quit",
}

func (r *repl) cmdHelp(context.Context, string) error {
	names := make([]string, 0, len(commands))
	for name := range commands {
		if _, alias := aliases[name]; !alias {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	r.printf("Commands:\n")
	for _, name := range names {
		helps := strings.Split(commands[name].help, "\n")
		for i, form := range strings.Split(commands[name].usage, "\n") {
			if i < len(helps) {
				r.printf("  %-36s %s\n", form, helps[i])
			} else {
				r.printf("  %s\n", form)
			}
		}
	}
	short := make([]string, 0, len(aliases))
	for alias := range aliases {
		short = append(short, alias)
	}
	sort.Strings(short)
	for i, alias := range short {
		short[i] = alias + " " + aliases[alias]
	}
	r.printf("Aliases: %s.\n", strings.Join(short, ", "))
	return nil
}

// selected returns the selected session, even one that has ended (its
// output and notifications can still be read).
func (r *repl) selected() (*dbgp.Session, error) {
	if s := r.session(); s != nil {
		return s, nil
	}
	return nil, errors.New("no session yet: start PHP with debugging enabled")
}

// live waits until the selected session can take commands: PHP has
// connected and the script is not running. After a session ends it waits
// for the next one, which the announcer selects. Ctrl-C cancels the wait.
func (r *repl) live(ctx context.Context) (*dbgp.Session, error) {
	waitingFor := ""
	for {
		s := r.session()
		switch {
		case s == nil || ended(s.State()):
			if !r.srv.Listening() {
				return nil, errors.New("no session, and not listening for PHP connections (use listen)")
			}
			if waitingFor != "connect" {
				waitingFor = "connect"
				r.printf("Waiting for PHP to connect (Ctrl-C to cancel)...\n")
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		case s.State().Status == dbgp.StatusRunning:
			if waitingFor != "stop" {
				waitingFor = "stop"
				r.printf("Waiting for the script to stop (Ctrl-C to cancel)...\n")
			}
			// Short waits, so a session switch with use is noticed.
			if _, err := s.Wait(ctx, 100*time.Millisecond); err != nil {
				return nil, err
			}
		default:
			if waitingFor == "stop" {
				r.showState(s, s.State())
			}
			return s, nil
		}
	}
}

func (r *repl) cmdMap(_ context.Context, args string) error {
	m, err := parseMapping(args)
	if err != nil {
		return usageError("map")
	}
	// parseMapping rejected anything AddPathMapping would; an error here
	// means a session rejected a breakpoint set again with the new path.
	err = r.srv.AddPathMapping(m)
	r.printf("Mapped %s => %s\n", m.Local, m.Remote)
	if err != nil {
		r.printf("Warning: %v\n", err)
	}
	return nil
}

func (r *repl) cmdMaps(context.Context, string) error {
	maps := r.srv.PathMap()
	if len(maps) == 0 {
		r.printf("No path mappings.\n")
	}
	for _, m := range maps {
		r.printf("%s => %s\n", m.Local, m.Remote)
	}
	if s := r.session(); s != nil && s.EngineMapping().Enabled {
		r.printf("Session %d's engine also maps paths itself, from .xdebug map files on the server.\n", s.ID())
	}
	return nil
}

func (r *repl) cmdListen(context.Context, string) error {
	if r.srv.Listening() {
		r.printf("Already listening on %s.\n", r.srv.Addr())
		return nil
	}
	if err := r.srv.StartListening(); err != nil {
		return err
	}
	r.printf("Listening on %s.\n", r.srv.Addr())
	return nil
}

func (r *repl) cmdUnlisten(context.Context, string) error {
	if !r.srv.Listening() {
		r.printf("Not listening.\n")
		return nil
	}
	if err := r.srv.StopListening(); err != nil {
		return err
	}
	r.printf("Stopped listening: new PHP connections are refused. Connected sessions carry on.\n")
	return nil
}

func (r *repl) cmdSessions(context.Context, string) error {
	if r.srv.Listening() {
		r.printf("Listening on %s.\n", r.srv.Addr())
	} else {
		r.printf("Not listening: PHP connections are refused (use listen).\n")
	}
	sessions := r.srv.Sessions()
	if len(sessions) == 0 {
		r.printf("No sessions.\n")
		return nil
	}
	cur := r.session()
	for _, s := range sessions {
		mark := " "
		if s == cur {
			mark = "*"
		}
		r.printf("%s %d  %-9s %s\n", mark, s.ID(), describeStatus(s.State()), s.Script())
	}
	return nil
}

// ended reports whether a session is over: stopped, or its connection
// closed. The engine reports stopped just before it closes.
func ended(st dbgp.State) bool {
	return st.Closed || st.Status == dbgp.StatusStopped
}

func describeStatus(st dbgp.State) string {
	if ended(st) {
		return "ended"
	}
	if st.Status == dbgp.StatusBreak {
		return fmt.Sprintf("line %d", st.Line)
	}
	return st.Status
}

func (r *repl) cmdUse(_ context.Context, args string) error {
	id, err := strconv.Atoi(args)
	if err != nil {
		return usageError("use")
	}
	s := r.srv.Session(id)
	if s == nil {
		return fmt.Errorf("no session %d", id)
	}
	r.mu.Lock()
	r.cur = s
	r.mu.Unlock()
	r.showState(s, s.State())
	return nil
}

func continueWith(command string) func(r *repl, ctx context.Context, args string) error {
	return func(r *repl, ctx context.Context, _ string) error {
		s, err := r.live(ctx)
		if err != nil {
			return err
		}
		st, err := s.Continue(ctx, command, forever)
		if errors.Is(err, context.Canceled) {
			r.printf("Interrupted; the script is still running. The next command waits for it to stop.\n")
			return nil
		}
		if err != nil {
			return err
		}
		r.showState(s, st)
		return nil
	}
}

// showState reports where a session is, with the source line and any new
// warnings when it has stopped.
func (r *repl) showState(s *dbgp.Session, st dbgp.State) {
	switch {
	case ended(st):
		r.printf("Session %d ended.\n", s.ID())
	case st.Status == dbgp.StatusRunning:
		r.printf("The script is running.\n")
	case st.Status == dbgp.StatusStarting:
		r.printf("Session %d is at the start of %s; use run or step.\n", s.ID(), s.Script())
	case st.Status == dbgp.StatusStopping:
		r.printf("Script finished; output shows its output, stop ends the session.\n")
	case st.Status == dbgp.StatusBreak:
		if st.Exception != "" {
			r.printf("Exception %s: %s\n", st.Exception, st.Message)
		}
		r.printf("Stopped at %s:%d\n", st.File, st.Line)
		if lines, err := r.sourceLines(s, st.File, st.Line, 0); err == nil {
			r.printf("%s", lines)
		}
	default:
		r.printf("Status: %s\n", st.Status)
	}
	r.showNewNotes(s)
}

func (r *repl) showNewNotes(s *dbgp.Session) {
	r.mu.Lock()
	from := r.notesFrom[s.ID()]
	r.mu.Unlock()
	notes, next := s.Notifications(from)
	r.mu.Lock()
	r.notesFrom[s.ID()] = next
	r.mu.Unlock()
	for _, n := range notes {
		// Resolved breakpoints are routine; notes still lists them.
		if n.Name != "breakpoint_resolved" {
			r.printf("%s\n", describeNote(n))
		}
	}
}

func describeNote(n dbgp.Notification) string {
	switch {
	case n.Name == "error" && n.Message != nil:
		return fmt.Sprintf("PHP %s: %s at %s:%d", n.Message.Type, strings.TrimSpace(n.Message.Text), n.Message.Filename, n.Message.Lineno)
	case n.Name == "breakpoint_resolved" && n.Breakpoint != nil:
		return fmt.Sprintf("Breakpoint resolved: %s:%d", n.Breakpoint.Filename, n.Breakpoint.Lineno)
	default:
		return "Notification: " + n.Name
	}
}

func (r *repl) cmdBreak(_ context.Context, args string) error {
	bp, err := r.parseBreakpoint(args)
	if err != nil {
		return err
	}
	added, err := r.srv.AddBreakpoint(bp)
	if added.ID == 0 {
		return err
	}
	r.printf("Breakpoint %d: %s\n", added.ID, describeBreakpoint(added))
	if err != nil {
		// Added, but a session rejected it.
		r.printf("Warning: %v\n", err)
	}
	return nil
}

// parseBreakpoint parses the break command's arguments.
func (r *repl) parseBreakpoint(args string) (dbgp.Breakpoint, error) {
	usage := usageError("break")
	spec, rest, _ := strings.Cut(args, " ")
	rest = strings.TrimSpace(rest)
	switch spec {
	case "":
		return dbgp.Breakpoint{}, usage
	case dbgp.BreakpointCall, dbgp.BreakpointReturn:
		if rest == "" {
			return dbgp.Breakpoint{}, usage
		}
		return dbgp.Breakpoint{Type: spec, Function: rest}, nil
	case dbgp.BreakpointException:
		if rest == "" {
			return dbgp.Breakpoint{}, usage
		}
		return dbgp.Breakpoint{Type: dbgp.BreakpointException, Exception: rest}, nil
	}

	bp := dbgp.Breakpoint{Type: dbgp.BreakpointLine}
	if cond, ok := strings.CutPrefix(rest, "if "); ok && strings.TrimSpace(cond) != "" {
		bp.Type, bp.Condition = dbgp.BreakpointConditional, strings.TrimSpace(cond)
	} else if rest != "" {
		return dbgp.Breakpoint{}, usage
	}
	file, line, err := parseLocation(spec)
	if err != nil {
		return dbgp.Breakpoint{}, usage
	}
	if file == "" {
		// A bare line number is in the file the session is stopped in.
		s := r.session()
		if s == nil || s.State().File == "" {
			return dbgp.Breakpoint{}, errors.New("break LINE needs a session stopped in a file; use FILE:LINE")
		}
		file = s.State().File
	}
	bp.File, bp.Line = file, line
	return bp, nil
}

// parseLocation parses [FILE:]LINE. FILE is made absolute; it is empty
// when only a line is given.
func parseLocation(spec string) (file string, line int, err error) {
	if sep := strings.LastIndex(spec, ":"); sep >= 0 {
		file, spec = spec[:sep], spec[sep+1:]
		if file == "" {
			return "", 0, fmt.Errorf("missing file before the line number")
		}
	}
	line, err = strconv.Atoi(spec)
	if err != nil || line <= 0 {
		return "", 0, fmt.Errorf("invalid line number %q", spec)
	}
	if file != "" && !filepath.IsAbs(file) && !strings.HasPrefix(file, "file://") {
		if file, err = filepath.Abs(file); err != nil {
			return "", 0, err
		}
	}
	return file, line, nil
}

func describeBreakpoint(bp dbgp.Breakpoint) string {
	switch bp.Type {
	case dbgp.BreakpointCall, dbgp.BreakpointReturn:
		return fmt.Sprintf("%s of %s", bp.Type, bp.Function)
	case dbgp.BreakpointException:
		return "exception " + bp.Exception
	case dbgp.BreakpointConditional:
		return fmt.Sprintf("%s:%d if %s", bp.File, bp.Line, bp.Condition)
	default:
		return fmt.Sprintf("%s:%d", bp.File, bp.Line)
	}
}

func (r *repl) cmdBreaks(context.Context, string) error {
	bps := r.srv.Breakpoints()
	if len(bps) == 0 {
		r.printf("No breakpoints.\n")
	}
	for _, bp := range bps {
		r.printf("%d  %s\n", bp.ID, describeBreakpoint(bp))
	}
	return nil
}

func (r *repl) cmdDelete(_ context.Context, args string) error {
	id, err := strconv.Atoi(args)
	if err != nil {
		return usageError("delete")
	}
	if err := r.srv.RemoveBreakpoint(id); err != nil {
		return err
	}
	r.printf("Deleted breakpoint %d.\n", id)
	return nil
}

func (r *repl) cmdStack(ctx context.Context, _ string) error {
	s, err := r.live(ctx)
	if err != nil {
		return err
	}
	frames, err := s.GetStack()
	if err != nil {
		return err
	}
	for _, line := range dbgp.FormatStack(frames) {
		r.printf("%s\n", line)
	}
	return nil
}

func (r *repl) cmdVars(ctx context.Context, args string) error {
	var nums [2]int
	for i, f := range strings.Fields(args) {
		n, err := strconv.Atoi(f)
		if i >= 2 || err != nil {
			return usageError("vars")
		}
		nums[i] = n
	}
	s, err := r.live(ctx)
	if err != nil {
		return err
	}
	props, err := s.GetContextProperties(nums[0], nums[1])
	if err != nil {
		return err
	}
	if len(props) == 0 {
		r.printf("No variables.\n")
	}
	compound := false
	for _, p := range props {
		value, isCompound := summarize(p)
		compound = compound || isCompound
		r.printf("%s\n", formatVariable(dbgp.Variable{Name: p.FullName, Type: p.Type, Value: value}))
	}
	if compound {
		r.printf("(print NAME shows what an array or object contains)\n")
	}
	return nil
}

// formatVariable formats a variable line. A variable without a value yet
// (DBGp type "uninitialized") is shown as PHP calls it, unset, with no
// value: showing null would look like it holds null.
func formatVariable(v dbgp.Variable) string {
	if v.Type != "uninitialized" {
		return dbgp.FormatVariable(v)
	}
	v.Type, v.Value = "unset", ""
	return strings.TrimRight(dbgp.FormatVariable(v), " =")
}

// summarize describes a variable without its contents: arrays by their
// number of elements, objects by their class. Children are left for print,
// so a large array does not flood the listing.
func summarize(p dbgp.Property) (value string, compound bool) {
	switch p.Type {
	case "array":
		return fmt.Sprintf("array[%d]", p.NumChildren), true
	case "object":
		if p.ClassName == "" {
			return "object", true
		}
		return p.ClassName, true
	}
	p.ChildProperties = nil
	return dbgp.ParseVariablesFromProperties([]dbgp.Property{p})[0].Value, false
}

func (r *repl) cmdContexts(ctx context.Context, _ string) error {
	s, err := r.live(ctx)
	if err != nil {
		return err
	}
	names, err := s.ContextNames(0)
	if err != nil {
		return err
	}
	for _, c := range names {
		r.printf("%d  %s\n", c.ID, c.Name)
	}
	return nil
}

func (r *repl) cmdPrint(ctx context.Context, args string) error {
	opts, name, ok := parsePropertyArgs(args, "-p", "-d")
	if !ok {
		return usageError("print")
	}
	s, err := r.live(ctx)
	if err != nil {
		return err
	}
	p, err := s.GetProperty(name, opts)
	if err != nil {
		return err
	}
	if p.Page >= p.Pages() {
		return fmt.Errorf("%s has %d pages", name, p.Pages())
	}
	more := ""
	if p.Page+1 < p.Pages() {
		more = fmt.Sprintf("print -p %d -d %d %s", p.Page+1, opts.Depth, name)
	}
	r.printProperty(*p, more)
	return nil
}

// parsePropertyArgs reads the allowed options (-p PAGE, -d DEPTH) before a
// property name.
func parsePropertyArgs(args string, allowed ...string) (opts dbgp.PropertyOptions, name string, ok bool) {
	targets := map[string]*int{"-p": &opts.Page, "-d": &opts.Depth}
	for {
		flag, rest, _ := strings.Cut(args, " ")
		target := targets[flag]
		if target == nil || !slices.Contains(allowed, flag) {
			break
		}
		value, rest, _ := strings.Cut(strings.TrimSpace(rest), " ")
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return opts, "", false
		}
		*target = n
		args = strings.TrimSpace(rest)
	}
	return opts, args, args != ""
}

// printProperty shows a property and one page of its children; more is the
// command that shows the next page, if there is one.
func (r *repl) printProperty(p dbgp.Property, more string) {
	for _, v := range dbgp.ParseVariablesFromProperties([]dbgp.Property{p}) {
		r.printf("%s\n", formatVariable(v))
	}
	if p.Pages() > 1 {
		r.printf("(page %d of %d", p.Page+1, p.Pages())
		if more != "" {
			r.printf("; %s for the next", more)
		}
		r.printf(")\n")
	}
	if p.Truncated() {
		r.printf("(value truncated to %d of %d bytes; use value for all of it)\n", len(mustDecode(p)), p.Size)
	}
}

func mustDecode(p dbgp.Property) string {
	v, _ := p.DecodedValue()
	return v
}

func (r *repl) cmdValue(ctx context.Context, args string) error {
	opts, name, ok := parsePropertyArgs(args, "-d")
	if !ok {
		return usageError("value")
	}
	s, err := r.live(ctx)
	if err != nil {
		return err
	}
	v, err := s.GetPropertyValue(name, opts)
	if err != nil {
		return err
	}
	r.printf("%s\n", v)
	return nil
}

func (r *repl) cmdEval(ctx context.Context, args string) error {
	if args == "" {
		return usageError("eval")
	}
	s, err := r.live(ctx)
	if err != nil {
		return err
	}
	p, err := s.EvalProperty(args, 0)
	if err != nil {
		return err
	}
	switch {
	case p == nil:
		r.printf("(no value)\n")
	case p.Type == "array" || p.Type == "object":
		// Eval results have no name; label the result with the code.
		p.FullName = args
		r.printProperty(*p, "")
	default:
		v, err := p.DecodedValue()
		if err != nil {
			return err
		}
		r.printf("%s (%s)\n", v, p.Type)
	}
	return nil
}

func (r *repl) cmdSet(ctx context.Context, args string) error {
	name, expr, ok := strings.Cut(args, "=")
	name, expr = strings.TrimSpace(name), strings.TrimSpace(expr)
	if !ok || name == "" || expr == "" {
		return usageError("set")
	}
	s, err := r.live(ctx)
	if err != nil {
		return err
	}
	if err := s.SetProperty(name, expr, dbgp.PropertyOptions{}); err != nil {
		return err
	}
	v, err := s.Eval(name)
	if err != nil {
		return err
	}
	r.printf("%s = %s\n", name, v)
	return nil
}

func (r *repl) cmdSource(ctx context.Context, args string) error {
	var file string
	var line int
	switch {
	case args == "":
	default:
		var err error
		if file, line, err = parseLocation(args); err != nil {
			return usageError("source")
		}
	}
	s, err := r.live(ctx)
	if err != nil {
		return err
	}
	// Default to where the session stopped: the file for a bare line,
	// and the line as well without arguments.
	st := s.State()
	if file == "" {
		file = st.File
		if line == 0 {
			line = st.Line
		}
	}
	if file == "" {
		file = s.Script()
	}
	if line == 0 {
		line = 1
	}
	lines, err := r.sourceLines(s, file, line, st.Line)
	if err != nil {
		return err
	}
	r.printf("%s", lines)
	return nil
}

// sourceLines formats source around line, marking the current line.
func (r *repl) sourceLines(s *dbgp.Session, file string, line, current int) (string, error) {
	if current == 0 {
		current = line
	}
	begin := max(1, line-sourceContext)
	src, err := s.GetSource(file, begin, line+sourceContext)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i, text := range strings.Split(strings.TrimRight(src, "\n"), "\n") {
		n := begin + i
		mark := "  "
		if n == current {
			mark = "=>"
		}
		fmt.Fprintf(&b, "%s %4d  %s\n", mark, n, text)
	}
	return b.String(), nil
}

func (r *repl) cmdOutput(context.Context, string) error {
	s, err := r.selected()
	if err != nil {
		return err
	}
	r.mu.Lock()
	from := r.outputFrom[s.ID()]
	r.mu.Unlock()
	text, next, truncated := s.Output(from)
	r.mu.Lock()
	r.outputFrom[s.ID()] = next
	r.mu.Unlock()
	if truncated {
		r.printf("(earlier output was dropped)\n")
	}
	if text == "" {
		r.printf("(no new output)\n")
		return nil
	}
	r.printf("%s", text)
	if !strings.HasSuffix(text, "\n") {
		r.printf("\n")
	}
	return nil
}

func (r *repl) cmdNotes(context.Context, string) error {
	s, err := r.selected()
	if err != nil {
		return err
	}
	r.mu.Lock()
	from := r.notesFrom[s.ID()]
	r.mu.Unlock()
	notes, next := s.Notifications(from)
	r.mu.Lock()
	r.notesFrom[s.ID()] = next
	r.mu.Unlock()
	if len(notes) == 0 {
		r.printf("(no new notifications)\n")
	}
	for _, n := range notes {
		r.printf("%s\n", describeNote(n))
	}
	return nil
}

func (r *repl) cmdStop(ctx context.Context, _ string) error {
	s, err := r.live(ctx)
	if err != nil {
		return err
	}
	if err := s.Stop(); err != nil {
		return err
	}
	r.printf("Stopped session %d.\n", s.ID())
	return nil
}

func (r *repl) cmdDetach(ctx context.Context, _ string) error {
	s, err := r.live(ctx)
	if err != nil {
		return err
	}
	if err := s.Detach(); err != nil {
		return err
	}
	r.printf("Detached session %d; the script runs on.\n", s.ID())
	return nil
}
