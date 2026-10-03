package dbgp

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// commandTimeout bounds how long a command waits for its response.
// Continuation commands are not bounded by it: see Session.Continue.
const commandTimeout = 30 * time.Second

// Continuation commands resume the script, which then runs until it stops
// again: at a breakpoint, after the step, or at the end of the script.
const (
	ContinueRun      = "run"
	ContinueStepInto = "step_into"
	ContinueStepOver = "step_over"
	ContinueStepOut  = "step_out"
)

// Output and notification history kept per session; older entries are
// dropped first.
const (
	maxOutputBytes   = 1 << 20
	maxNotifications = 1000
)

var (
	// ErrRunning is returned for commands sent while the script runs: the
	// engine only reads commands while it is stopped.
	ErrRunning = errors.New("session is running; wait for it to stop")

	errConnectionClosed = errors.New("connection closed")
)

// State is where a session is: the engine status and, when stopped at a
// break, the location and any exception.
type State struct {
	Status    string // starting, running, break, stopping or stopped
	Reason    string // ok, error, aborted or exception
	File      string // local path where execution stopped (Status break)
	Line      int
	Exception string // exception class, when stopped on an exception
	Message   string // exception message
	Error     string // the engine's error, if the last continuation failed
	Closed    bool   // the connection has ended
}

// Session is one debugging connection from the engine: one PHP request or
// script run.
type Session struct {
	id     int
	server *Server
	conn   net.Conn
	init   *InitPacket
	closed chan struct{} // closed when the read loop exits

	// cmdSlot serializes commands: at most one is in flight, so an error
	// response without a transaction_id belongs to it.
	cmdSlot chan struct{}

	// syncMu serializes breakpoint syncs; it guards bpIDs, which maps
	// server breakpoint ids to this engine's breakpoint ids.
	syncMu sync.Mutex
	bpIDs  map[int]int

	mu         sync.Mutex
	transID    int
	pending    *pendingCommand
	contID     int   // transaction id of the in-flight continuation
	contPrev   State // state to restore if the continuation is rejected
	state      State
	changed    chan struct{}
	output     []byte // most recent program output
	outputBase int    // stream offset of output[0]
	notes      []Notification
	notesBase  int // index of notes[0] among all notifications
	setupErr   error
}

// pendingCommand is a command waiting for its response.
type pendingCommand struct {
	id int
	ch chan *Response
}

// newSession starts reading responses for an engine connection whose init
// packet has been read.
func newSession(id int, server *Server, conn net.Conn, reader *bufio.Reader, init *InitPacket) *Session {
	s := &Session{
		id:      id,
		server:  server,
		conn:    conn,
		init:    init,
		closed:  make(chan struct{}),
		cmdSlot: make(chan struct{}, 1),
		bpIDs:   map[int]int{},
		state:   State{Status: StatusStarting},
		changed: make(chan struct{}),
	}
	go s.readLoop(reader)
	return s
}

// ID identifies the session within its server.
func (s *Session) ID() int { return s.id }

// Init returns the session's init packet.
func (s *Session) Init() *InitPacket { return s.init }

// State returns the session's current state without asking the engine.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Done is closed when the connection has ended.
func (s *Session) Done() <-chan struct{} { return s.closed }

// SetupError reports features from Config.Features the engine rejected.
func (s *Session) SetupError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setupErr
}

// Output returns program output from stream offset from onwards, and the
// offset to pass next time. truncated reports that output before the
// returned text was dropped to bound memory.
func (s *Session) Output(from int) (text string, next int, truncated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next = s.outputBase + len(s.output)
	if from < s.outputBase {
		from, truncated = s.outputBase, true
	}
	if from > next {
		from = next
	}
	return string(s.output[from-s.outputBase:]), next, truncated
}

// Notifications returns notifications from index from onwards, and the
// index to pass next time.
func (s *Session) Notifications(from int) ([]Notification, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.notesBase + len(s.notes)
	if from < s.notesBase {
		from = s.notesBase
	}
	if from > next {
		from = next
	}
	return append([]Notification(nil), s.notes[from-s.notesBase:]...), next
}

// readPacket reads a single DBGp packet (length + NULL + data + NULL)
func readPacket(reader *bufio.Reader) ([]byte, error) {
	// Read length until NULL
	line, err := reader.ReadString('\x00')
	if err != nil {
		return nil, err
	}

	// Parse length
	line = strings.TrimSuffix(line, "\x00")
	length, err := strconv.Atoi(line)
	if err != nil {
		return nil, fmt.Errorf("parse packet length: %w", err)
	}
	if length < 0 {
		return nil, fmt.Errorf("negative packet length %d", length)
	}

	// Read exact number of bytes
	data := make([]byte, length)
	_, err = io.ReadFull(reader, data)
	if err != nil {
		return nil, err
	}

	// Read trailing NULL
	b, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if b != '\x00' {
		return nil, fmt.Errorf("expected NULL terminator, got %x", b)
	}

	return data, nil
}

// readLoop reads packets until the connection ends, then marks the session
// closed, which fails pending and later commands.
func (s *Session) readLoop(reader *bufio.Reader) {
	defer s.finish()
	for {
		data, err := readPacket(reader)
		if err != nil {
			return
		}
		kind, err := packetKind(data)
		if err != nil {
			continue
		}
		switch kind {
		case "response":
			if resp, err := ParseResponse(data); err == nil {
				s.dispatch(resp)
			}
		case "stream":
			if _, text, err := ParseStream(data); err == nil {
				s.appendOutput(text)
			}
		case "notify":
			if n, err := ParseNotification(data); err == nil {
				s.addNotification(*n)
			}
		}
	}
}

func (s *Session) finish() {
	s.mu.Lock()
	st := s.state
	st.Status, st.Closed = StatusStopped, true
	s.contID = 0
	s.setStateLocked(st)
	s.mu.Unlock()
	close(s.closed)
}

// dispatch routes a response to the in-flight continuation or command.
// Errors the engine sends without a transaction_id (commands it could not
// parse) belong to whichever is in flight: there is only ever one.
func (s *Session) dispatch(resp *Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	untaggedError := resp.Transaction == 0 && resp.Error != nil

	if s.contID != 0 && (resp.Transaction == s.contID || (untaggedError && s.pending == nil)) {
		s.contID = 0
		s.setStateLocked(continuationState(resp, s.contPrev))
		return
	}

	p := s.pending
	if p == nil || (resp.Transaction != p.id && !untaggedError) {
		return
	}
	s.pending = nil
	p.ch <- resp
	// stop, detach and status report the engine status.
	if resp.Status != "" && resp.Status != s.state.Status {
		st := s.state
		st.Status, st.Reason = resp.Status, resp.Reason
		if st.Status != StatusBreak {
			st.File, st.Line, st.Exception, st.Message = "", 0, "", ""
		}
		s.setStateLocked(st)
	}
}

// continuationState is the state a continuation response leaves the session in.
func continuationState(resp *Response, prev State) State {
	if resp.Error != nil {
		prev.Error = fmt.Sprintf("error %d: %s", resp.Error.Code, resp.Error.Message)
		return prev
	}
	st := State{Status: resp.Status, Reason: resp.Reason}
	if st.Status == StatusBreak && resp.Message != nil {
		st.File, st.Line = resp.ParseMessage()
		st.Exception = resp.Message.Exception
		st.Message = strings.TrimSpace(resp.Message.Text)
	}
	return st
}

func (s *Session) setStateLocked(st State) {
	s.state = st
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Session) appendOutput(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.output = append(s.output, text...)
	if over := len(s.output) - maxOutputBytes; over > 0 {
		s.output = append([]byte(nil), s.output[over:]...)
		s.outputBase += over
	}
}

func (s *Session) addNotification(n Notification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, n)
	if over := len(s.notes) - maxNotifications; over > 0 {
		s.notes = append([]Notification(nil), s.notes[over:]...)
		s.notesBase += over
	}
}

// formatCommand builds a DBGp command line:
//
//	name -a value ... -i id [-- base64(data)]
//
// args holds option/value pairs such as "-f", uri. Data is sent only when
// non-nil and always last, since the engine reads everything after "--" as
// data.
func formatCommand(name string, id int, args []string, data []byte) (string, error) {
	if name == "" || strings.ContainsAny(name, " \x00") {
		return "", fmt.Errorf("invalid command name %q", name)
	}
	if len(args)%2 != 0 {
		return "", fmt.Errorf("%s: odd number of option arguments", name)
	}

	var b strings.Builder
	b.WriteString(name)
	for i := 0; i < len(args); i += 2 {
		opt, value := args[i], args[i+1]
		if len(opt) != 2 || opt[0] != '-' || opt[1] < 'a' || opt[1] > 'z' || opt == "-i" {
			return "", fmt.Errorf("%s: invalid option %q", name, opt)
		}
		if strings.ContainsRune(value, 0) {
			return "", fmt.Errorf("%s: option %s contains a NUL byte", name, opt)
		}
		b.WriteString(" " + opt + " " + quoteArg(value))
	}
	b.WriteString(" -i " + strconv.Itoa(id))
	if data != nil {
		b.WriteString(" -- " + base64.StdEncoding.EncodeToString(data))
	}
	return b.String(), nil
}

// quoteArg quotes an option value when needed. Unquoted values end at the
// first space; quoted values are unescaped C-style by the engine.
func quoteArg(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\r\n\"\\") {
		return value
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

// acquire takes the command slot, waiting for any command in flight.
func (s *Session) acquire(name string, timeout <-chan time.Time) error {
	select {
	case s.cmdSlot <- struct{}{}:
		return nil
	case <-s.closed:
		return errConnectionClosed
	case <-timeout:
		return fmt.Errorf("%s: timeout waiting for previous command", name)
	}
}

func (s *Session) release() { <-s.cmdSlot }

// checkCanSendLocked reports why no command can be sent now, if so.
func (s *Session) checkCanSendLocked() error {
	if s.state.Closed {
		return errConnectionClosed
	}
	if s.state.Status == StatusRunning {
		return ErrRunning
	}
	return nil
}

// sendCommand sends a command and waits for its response. Commands are
// serialized, so this also waits for any command already in flight.
func (s *Session) sendCommand(name string, args []string, data []byte) (*Response, error) {
	timeout := time.NewTimer(commandTimeout)
	defer timeout.Stop()
	if err := s.acquire(name, timeout.C); err != nil {
		return nil, err
	}
	defer s.release()

	s.mu.Lock()
	if err := s.checkCanSendLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.transID++
	line, err := formatCommand(name, s.transID, args, data)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	p := &pendingCommand{id: s.transID, ch: make(chan *Response, 1)}
	s.pending = p
	s.mu.Unlock()

	if _, err := s.conn.Write([]byte(line + "\x00")); err != nil {
		s.clearPending(p)
		return nil, fmt.Errorf("send command: %w", err)
	}

	select {
	case resp := <-p.ch:
		return responseResult(resp)
	case <-s.closed:
		s.clearPending(p)
		// The response may have arrived just before the connection closed.
		select {
		case resp := <-p.ch:
			return responseResult(resp)
		default:
			return nil, errConnectionClosed
		}
	case <-timeout.C:
		s.clearPending(p)
		return nil, fmt.Errorf("%s: timeout waiting for response", name)
	}
}

func (s *Session) clearPending(p *pendingCommand) {
	s.mu.Lock()
	if s.pending == p {
		s.pending = nil
	}
	s.mu.Unlock()
}

// responseResult turns an engine error response into a Go error.
func responseResult(resp *Response) (*Response, error) {
	if resp.Error != nil {
		return resp, fmt.Errorf("error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp, nil
}

// Continue resumes the script with a continuation command (ContinueRun,
// ContinueStepInto, ContinueStepOver or ContinueStepOut) and waits up to
// wait for it to stop. If it is still running then, the returned state has
// Status running: call Wait to keep waiting. Breakpoints added to the server
// are applied first.
func (s *Session) Continue(ctx context.Context, command string, wait time.Duration) (State, error) {
	switch command {
	case ContinueRun, ContinueStepInto, ContinueStepOver, ContinueStepOut:
	default:
		return s.State(), fmt.Errorf("%q is not a continuation command", command)
	}
	if err := s.syncBreakpoints(); err != nil {
		return s.State(), err
	}
	if err := s.startContinuation(command); err != nil {
		return s.State(), err
	}
	st, err := s.Wait(ctx, wait)
	if err == nil && st.Error != "" {
		err = fmt.Errorf("%s: %s", command, st.Error)
	}
	return st, err
}

// startContinuation sends a continuation command without waiting for the
// script to stop; the response arrives when it does.
func (s *Session) startContinuation(command string) error {
	timeout := time.NewTimer(commandTimeout)
	defer timeout.Stop()
	if err := s.acquire(command, timeout.C); err != nil {
		return err
	}
	defer s.release()

	s.mu.Lock()
	if err := s.checkCanSendLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.transID++
	id := s.transID
	line, err := formatCommand(command, id, nil, nil)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	prev := s.state
	prev.Error = ""
	s.contID, s.contPrev = id, prev
	s.setStateLocked(State{Status: StatusRunning, Reason: "ok"})
	s.mu.Unlock()

	if _, err := s.conn.Write([]byte(line + "\x00")); err != nil {
		s.mu.Lock()
		if s.contID == id {
			s.contID = 0
			s.setStateLocked(prev)
		}
		s.mu.Unlock()
		return fmt.Errorf("send command: %w", err)
	}
	return nil
}

// Wait waits up to wait for a running script to stop and returns the
// session state, which has Status running if it has not stopped yet.
func (s *Session) Wait(ctx context.Context, wait time.Duration) (State, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		s.mu.Lock()
		st, changed := s.state, s.changed
		s.mu.Unlock()
		if st.Status != StatusRunning {
			return st, nil
		}
		select {
		case <-changed:
		case <-timer.C:
			return st, nil
		case <-ctx.Done():
			return st, ctx.Err()
		}
	}
}

// syncBreakpoints makes the engine's breakpoints match the server's: it
// sets breakpoints added since the last sync and removes deleted ones.
func (s *Session) syncBreakpoints() error {
	if s.server == nil {
		return nil
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	var errs []error
	want := map[int]bool{}
	for _, bp := range s.server.Breakpoints() {
		want[bp.ID] = true
		if _, ok := s.bpIDs[bp.ID]; ok {
			continue
		}
		id, err := s.SetBreakpointSpec(bp)
		if err != nil {
			errs = append(errs, fmt.Errorf("breakpoint %d: %w", bp.ID, err))
			continue
		}
		s.bpIDs[bp.ID] = id
	}
	for serverID, engineID := range s.bpIDs {
		if want[serverID] {
			continue
		}
		// Forget it even if removal fails: it may already be gone (e.g. a
		// temporary breakpoint), and a closed session has none.
		delete(s.bpIDs, serverID)
		if err := s.RemoveBreakpoint(engineID); err != nil && !errors.Is(err, errConnectionClosed) {
			errs = append(errs, fmt.Errorf("remove breakpoint %d: %w", serverID, err))
		}
	}
	return errors.Join(errs...)
}

// SetBreakpointSpec sets a breakpoint of any type on this session's engine
// only, and returns the engine's breakpoint id. Breakpoints that should
// apply to every session belong on the server (Server.AddBreakpoint).
func (s *Session) SetBreakpointSpec(bp Breakpoint) (int, error) {
	if err := bp.validate(); err != nil {
		return 0, err
	}
	args := []string{"-t", bp.typeName()}
	var data []byte
	switch bp.typeName() {
	case BreakpointLine, BreakpointConditional:
		args = append(args, "-f", MakeFileURI(bp.File), "-n", strconv.Itoa(bp.Line))
		if bp.typeName() == BreakpointConditional {
			data = []byte(bp.Condition)
		}
	case BreakpointCall, BreakpointReturn:
		args = append(args, "-m", bp.Function)
	case BreakpointException:
		args = append(args, "-x", bp.Exception)
	}
	resp, err := s.sendCommand("breakpoint_set", args, data)
	if err != nil {
		return 0, err
	}
	return resp.BreakpointID, nil
}

// SetBreakpoint sets a line breakpoint
func (s *Session) SetBreakpoint(file string, line int) (int, error) {
	return s.SetBreakpointSpec(Breakpoint{Type: BreakpointLine, File: file, Line: line})
}

// SetConditionalBreakpoint sets a conditional breakpoint
func (s *Session) SetConditionalBreakpoint(file string, line int, condition string) (int, error) {
	return s.SetBreakpointSpec(Breakpoint{Type: BreakpointConditional, File: file, Line: line, Condition: condition})
}

// RemoveBreakpoint removes a breakpoint
func (s *Session) RemoveBreakpoint(id int) error {
	_, err := s.sendCommand("breakpoint_remove", []string{"-d", strconv.Itoa(id)}, nil)
	return err
}

// ListBreakpoints lists all breakpoints
func (s *Session) ListBreakpoints() ([]BreakpointInfo, error) {
	resp, err := s.sendCommand("breakpoint_list", nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Breakpoints, nil
}

// Stop ends the script. The engine closes the connection afterwards.
func (s *Session) Stop() error {
	_, err := s.sendCommand("stop", nil, nil)
	return err
}

// Detach stops debugging and lets the script run to completion.
func (s *Session) Detach() error {
	_, err := s.sendCommand("detach", nil, nil)
	return err
}

// GetStack returns the call stack
func (s *Session) GetStack() ([]StackFrame, error) {
	resp, err := s.sendCommand("stack_get", nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Stack, nil
}

// GetContext returns variables at the given depth and context
func (s *Session) GetContext(depth int, context int) ([]Variable, error) {
	args := []string{"-d", strconv.Itoa(depth), "-c", strconv.Itoa(context)}
	resp, err := s.sendCommand("context_get", args, nil)
	if err != nil {
		return nil, err
	}
	return ParseVariablesFromProperties(resp.Properties), nil
}

// Eval evaluates an expression and returns its value. Strings and numbers
// are returned as-is; arrays and objects in summary form, e.g. "array[3]".
func (s *Session) Eval(code string) (string, error) {
	resp, err := s.sendCommand("eval", nil, []byte(code))
	if err != nil {
		return "", err
	}
	if len(resp.Properties) == 0 {
		return "", nil
	}
	p := resp.Properties[0]
	if p.Type == "array" || p.Type == "object" {
		return formatPropertyValue(p), nil
	}
	return decodeEncoded(p.Encoding, p.Value)
}

// GetSource returns source code
func (s *Session) GetSource(file string, begin, end int) (string, error) {
	args := []string{"-f", MakeFileURI(file)}
	if begin > 0 {
		args = append(args, "-b", strconv.Itoa(begin))
	}
	if end > 0 {
		args = append(args, "-e", strconv.Itoa(end))
	}
	resp, err := s.sendCommand("source", args, nil)
	if err != nil {
		return "", err
	}
	return decodeResponseValue(resp)
}

// Status asks the engine for its status
func (s *Session) Status() (string, error) {
	resp, err := s.sendCommand("status", nil, nil)
	if err != nil {
		return "", err
	}
	return resp.Status, nil
}

// FeatureGet gets a feature value. It fails for features the engine does
// not support.
func (s *Session) FeatureGet(name string) (string, error) {
	if strings.ContainsAny(name, " \t\r\n") {
		return "", fmt.Errorf("feature name must not contain whitespace")
	}
	resp, err := s.sendCommand("feature_get", []string{"-n", name}, nil)
	if err != nil {
		return "", err
	}
	if resp.Supported == "0" {
		return "", fmt.Errorf("feature %s is not supported", name)
	}
	return decodeResponseValue(resp)
}

// FeatureSet sets a feature value
func (s *Session) FeatureSet(name, value string) error {
	if strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("feature name must not contain whitespace")
	}
	_, err := s.sendCommand("feature_set", []string{"-n", name, "-v", value}, nil)
	return err
}

// Close drops the connection. A script that is still running continues
// without the debugger.
func (s *Session) Close() error {
	return s.conn.Close()
}

func decodeResponseValue(resp *Response) (string, error) {
	value := strings.TrimSpace(resp.Value)
	if value == "" {
		value = strings.TrimSpace(resp.Raw)
	}
	if value == "" {
		return "", nil
	}
	decoded, err := decodeEncoded(resp.Encoding, value)
	if err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return decoded, nil
}
