package dbgp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
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
	maxOutputBytes       = 1 << 20
	maxNotifications     = 1000
	maxNotificationBytes = 1 << 20
	// maxMessageBytes bounds the text kept of one notification's message,
	// such as a PHP warning: real ones, even with a stack trace, are
	// shorter.
	maxMessageBytes = 8 << 10
)

// MaxValueBytes is the most of a value GetProperty and GetPropertyValue
// ask the engine for, whatever PropertyOptions.MaxData says: the engine
// sends it in one packet, base64-encoded, which must stay under the packet
// size limit.
const MaxValueBytes = 20 << 20

// maxEndedSessions is how many ended sessions a Server keeps; older ones
// are forgotten.
const maxEndedSessions = 50

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
	// connCtx is cancelled when conn is closed.
	connCtx context.Context

	// cmdSlot serializes commands: at most one is in flight, so an error
	// response without a transaction_id belongs to it.
	cmdSlot chan struct{}

	// syncMu serializes breakpoint syncs; it guards bpIDs, which maps
	// server breakpoint ids to this engine's breakpoint ids, and rejected.
	syncMu sync.Mutex
	bpIDs  map[int]int
	// rejected holds the server breakpoints the engine refused, by server
	// id, so that syncs do not send them again, and so fail only once.
	rejected map[int]*rejection
	// resync asks the next sync to set every breakpoint again, because a
	// path mapping changed.
	resync atomic.Bool

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
	notesBytes int // size of notes, see Notification.size
	setupErr   error
	warnings   []string
	engineMap  EngineMapping
}

// EngineMapping describes the engine's own path mapping: map files in
// .xdebug directories on the server, enabled by the path_mapping setting.
// With it, the engine takes and returns local paths itself, except for the
// source command.
type EngineMapping struct {
	Detected     bool   // false if the engine could not be asked
	Enabled      bool   // the engine's path_mapping setting is on
	RemoteScript string // the entry script's path on the server
}

// engineMappingProbe asks for the path_mapping settings (xdebug.* and
// php_debugger.* are separate entries with one value) and the entry
// script's server path. It only reads.
const engineMappingProbe = `json_encode([ini_get('xdebug.path_mapping'), ini_get('php_debugger.path_mapping'), get_included_files()[0] ?? ''])`

func (s *Session) detectEngineMapping(paths PathMap) {
	p, err := s.EvalProperty(engineMappingProbe, 0)
	if err != nil || p == nil || p.Truncated() {
		return
	}
	encoded, err := p.DecodedValue()
	if err != nil {
		return
	}
	// ini_get returns false for settings an engine does not have.
	var probe [3]any
	if json.Unmarshal([]byte(encoded), &probe) != nil {
		return
	}
	remote, _ := probe[2].(string)
	m := EngineMapping{Detected: true, Enabled: iniEnabled(probe[0]) || iniEnabled(probe[1]), RemoteScript: remote}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.engineMap = m
	if m.Enabled && len(paths) > 0 {
		s.warnings = append(s.warnings, "the engine maps paths itself (path_mapping is on) and Config.PathMap is also set: "+
			"paths both could translate are mapped by the client, so the engine's line-range rules do not apply to them")
	}
}

// iniEnabled reports whether an ini_get value means a boolean setting is on.
func iniEnabled(v any) bool {
	s, _ := v.(string)
	switch strings.ToLower(s) {
	case "1", "on", "yes", "true":
		return true
	}
	return false
}

// EngineMapping reports whether the engine maps paths itself.
func (s *Session) EngineMapping() EngineMapping {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engineMap
}

// SetupWarnings reports problems found while setting up the session that
// do not stop it from working, such as path mapping configured twice.
func (s *Session) SetupWarnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.warnings...)
}

// rejection is a breakpoint the engine refused, and whether that has been
// reported.
type rejection struct {
	err      error
	reported bool
}

// pendingCommand is a command waiting for its response.
type pendingCommand struct {
	id int
	ch chan *Response
}

// newSession starts reading responses for an engine connection whose init
// packet has been read.
func newSession(id int, server *Server, conn net.Conn, reader *bufio.Reader, init *InitPacket) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		id:       id,
		server:   server,
		conn:     &sessionConn{Conn: conn, cancel: cancel},
		connCtx:  ctx,
		init:     init,
		closed:   make(chan struct{}),
		cmdSlot:  make(chan struct{}, 1),
		bpIDs:    map[int]int{},
		rejected: map[int]*rejection{},
		state:    State{Status: StatusStarting},
		changed:  make(chan struct{}),
	}
	go s.readLoop(reader)
	return s
}

// sessionConn is a session's connection, whose Close also cancels the
// context the session reads with, so a read waiting for its turn ends too.
type sessionConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *sessionConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}

// ID identifies the session within its server.
func (s *Session) ID() int { return s.id }

// Init returns the session's init packet.
func (s *Session) Init() *InitPacket { return s.init }

// RemoteAddr returns the address the engine connected from.
func (s *Session) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }

// pathMap returns the server's current path map.
func (s *Session) pathMap() PathMap {
	if s.server == nil {
		return nil
	}
	return s.server.pathMap()
}

// Script returns the local path of the script being debugged.
func (s *Session) Script() string { return s.pathMap().localPath(s.init.FileURI) }

// State returns the session's current state without asking the engine.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Done is closed when the connection has ended.
func (s *Session) Done() <-chan struct{} { return s.closed }

// ended reports whether the connection has ended.
func (s *Session) ended() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

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

// Packet size limits. The length comes from whoever connected, so a bad one
// must not crash dbgp or make it allocate more than the data that arrives.
const (
	// maxPacketBytes bounds every packet. Responses can be large, such as a
	// long string from property_value (see MaxValueBytes), but not larger.
	// A larger packet ends the session.
	maxPacketBytes = 32 << 20
	// maxInitPacketBytes bounds the init packet, which a connection sends
	// before it is known to be an engine. Real ones are well under 1 KB.
	maxInitPacketBytes = 64 << 10
	// packetChunk is how much of a packet is allocated ahead of the data.
	packetChunk = 64 << 10
)

// readPacket reads a single DBGp packet, length NUL data NUL, of at most
// limit bytes of data.
func readPacket(reader *bufio.Reader, limit int) ([]byte, error) {
	length, err := readPacketLength(reader, limit)
	if err != nil {
		return nil, err
	}
	return readPacketData(reader, length)
}

// readPacketData reads the length bytes of a packet's data and the NUL
// that ends it.
func readPacketData(reader *bufio.Reader, length int) ([]byte, error) {
	// Grow the buffer as the data arrives rather than trusting the length.
	var data bytes.Buffer
	data.Grow(min(length, packetChunk))
	if _, err := io.CopyN(&data, reader, int64(length)); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}

	b, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if b != '\x00' {
		return nil, fmt.Errorf("expected NULL terminator, got %x", b)
	}
	return data.Bytes(), nil
}

// readPacketLength reads the decimal length that starts a packet and its
// NUL terminator. It stops at the first byte that cannot be part of a valid
// length, so a sender cannot make it read an endless prefix.
func readPacketLength(reader *bufio.Reader, limit int) (int, error) {
	maxDigits := len(strconv.Itoa(limit))
	var length int64
	for digits := 0; ; digits++ {
		b, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		switch {
		case b == '\x00' && digits > 0:
			return int(length), nil
		case b < '0' || b > '9':
			return 0, fmt.Errorf("invalid packet length: unexpected byte %q", b)
		case digits == maxDigits:
			return 0, fmt.Errorf("invalid packet length: more than %d digits", maxDigits)
		}
		length = length*10 + int64(b-'0')
		if length > int64(limit) {
			return 0, fmt.Errorf("packet length exceeds the limit of %d bytes", limit)
		}
	}
}

// readLoop reads packets until the connection ends, then marks the session
// closed, which fails pending and later commands.
func (s *Session) readLoop(reader *bufio.Reader) {
	defer s.finish()
	for {
		length, err := readPacketLength(reader, maxPacketBytes)
		if err != nil {
			return
		}
		// Reading and parsing a packet takes a few times its size: the
		// server bounds how much its sessions read at once.
		release, err := s.server.reservePacket(s.connCtx, length)
		if err != nil {
			return
		}
		data, err := readPacketData(reader, length)
		if err != nil {
			release()
			return
		}
		s.handlePacket(data)
		release()
	}
}

// handlePacket parses a packet from the engine and acts on it.
func (s *Session) handlePacket(data []byte) {
	kind, err := packetKind(data)
	if err != nil {
		return
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
		s.setStateLocked(continuationState(resp, s.contPrev, s.pathMap()))
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
func continuationState(resp *Response, prev State, paths PathMap) State {
	if resp.Error != nil {
		prev.Error = fmt.Sprintf("error %d: %s", resp.Error.Code, resp.Error.Message)
		return prev
	}
	st := State{Status: resp.Status, Reason: resp.Reason}
	if st.Status == StatusBreak && resp.Message != nil {
		st.File, st.Line = paths.localPath(resp.Message.Filename), resp.Message.Lineno
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
	if n.Message != nil {
		n.Message.Filename = s.pathMap().localPath(n.Message.Filename)
		n.Message.Text = cutUTF8(n.Message.Text, maxMessageBytes)
	}
	if n.Breakpoint != nil && n.Breakpoint.Filename != "" {
		n.Breakpoint.Filename = s.pathMap().localPath(n.Breakpoint.Filename)
	}
	size := n.size()
	if size > maxNotificationBytes/16 {
		return // only a malicious engine sends one this large
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, n)
	s.notesBytes += size
	over := 0
	for len(s.notes)-over > maxNotifications || s.notesBytes > maxNotificationBytes {
		s.notesBytes -= s.notes[over].size()
		over++
	}
	if over > 0 {
		s.notes = append([]Notification(nil), s.notes[over:]...)
		s.notesBase += over
	}
}

// size approximates the memory a notification takes: its strings, and a
// little for the rest.
func (n Notification) size() int {
	size := 64 + len(n.Name)
	if m := n.Message; m != nil {
		size += len(m.Filename) + len(m.Exception) + len(m.Type) + len(m.Code) + len(m.Text)
	}
	if b := n.Breakpoint; b != nil {
		size += len(b.Type) + len(b.Filename) + len(b.Function) + len(b.State) + len(b.Resolved) +
			len(b.Exception) + len(b.HitCondition) + len(b.Expression)
	}
	return size
}

// cutUTF8 returns at most the first n bytes of s, ending on a whole
// character.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
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

// EngineError is an error response from the engine, such as code 300 for
// a property that does not exist.
type EngineError struct {
	Command string
	Code    int
	Message string
}

func (e *EngineError) Error() string {
	return fmt.Sprintf("error %d: %s", e.Code, e.Message)
}

// responseResult turns an engine error response into an *EngineError.
func responseResult(resp *Response) (*Response, error) {
	if resp.Error != nil {
		return resp, &EngineError{Command: resp.Command, Code: resp.Error.Code, Message: resp.Error.Message}
	}
	return resp, nil
}

// Continue resumes the script with a continuation command (ContinueRun,
// ContinueStepInto, ContinueStepOver or ContinueStepOut) and waits up to
// wait for it to stop. If it is still running then, the returned state has
// Status running: call Wait to keep waiting. Breakpoints added to the server
// are applied first. If that fails, the script is not resumed: a breakpoint
// the engine rejected is reported once, and the next Continue runs without
// it.
func (s *Session) Continue(ctx context.Context, command string, wait time.Duration) (State, error) {
	switch command {
	case ContinueRun, ContinueStepInto, ContinueStepOver, ContinueStepOut:
	default:
		return s.State(), fmt.Errorf("%q is not a continuation command", command)
	}
	if err := s.syncBreakpoints(true); err != nil {
		return s.State(), fmt.Errorf("%w; the script was not resumed: "+
			"continue again to run it without the breakpoints the engine rejected", err)
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

// finished reports whether the script has finished, so breakpoints no
// longer matter: the engine refuses breakpoint commands once it is stopping
// or stopped, and a closed session has none.
func (s *Session) finished() bool {
	st := s.State()
	return st.Closed || st.Status == StatusStopping || st.Status == StatusStopped
}

// syncBreakpoints makes the engine's breakpoints match the server's: it
// sets breakpoints added since the last sync and removes deleted ones. A
// breakpoint the engine rejects is not sent again until a path mapping
// changes; its error is returned once, by the first sync that reports.
// Other errors, such as a timeout, are returned until the breakpoint is set.
func (s *Session) syncBreakpoints(report bool) error {
	if s.server == nil || s.finished() {
		return nil
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	var errs []error
	if s.resync.Swap(false) {
		// A path mapping changed: set every breakpoint again, including
		// rejected ones, which the new paths may fix.
		for serverID, engineID := range s.bpIDs {
			delete(s.bpIDs, serverID)
			if err := s.RemoveBreakpoint(engineID); err != nil && !errors.Is(err, errConnectionClosed) {
				errs = append(errs, fmt.Errorf("remove breakpoint %d: %w", serverID, err))
			}
		}
		clear(s.rejected)
	}
	want := map[int]bool{}
	for _, bp := range s.server.Breakpoints() {
		want[bp.ID] = true
		if _, ok := s.bpIDs[bp.ID]; ok {
			continue
		}
		if r, ok := s.rejected[bp.ID]; ok {
			if report && !r.reported {
				r.reported = true
				errs = append(errs, r.err)
			}
			continue
		}
		id, err := s.SetBreakpointSpec(bp)
		if err != nil {
			err = fmt.Errorf("breakpoint %d: %w", bp.ID, err)
			if engineErr := (*EngineError)(nil); errors.As(err, &engineErr) {
				s.rejected[bp.ID] = &rejection{err: err, reported: report}
			}
			errs = append(errs, err)
			continue
		}
		s.bpIDs[bp.ID] = id
	}
	for serverID := range s.rejected {
		if !want[serverID] {
			delete(s.rejected, serverID)
		}
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
		args = append(args, "-f", s.pathMap().engineURI(bp.File), "-n", strconv.Itoa(bp.Line))
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

// ListBreakpoints lists the engine's breakpoints, with local paths.
func (s *Session) ListBreakpoints() ([]BreakpointInfo, error) {
	resp, err := s.sendCommand("breakpoint_list", nil, nil)
	if err != nil {
		return nil, err
	}
	for i, bp := range resp.Breakpoints {
		if bp.Filename != "" {
			resp.Breakpoints[i].Filename = s.pathMap().localPath(bp.Filename)
		}
	}
	return resp.Breakpoints, nil
}

// Stop ends the script and the session. Stopped mid-script, the engine
// waits for the IDE to close the connection before PHP exits, so Stop closes
// it, and returns once the session has ended. A running script reads no
// commands, so Stop cannot end it and returns ErrRunning; Detach can let it
// go.
func (s *Session) Stop() error {
	if _, err := s.sendCommand("stop", nil, nil); err != nil {
		return err
	}
	_ = s.conn.Close()
	<-s.closed
	return nil
}

// Detach stops debugging and lets the script run to completion. The engine
// reads commands only while the script is stopped, so a running script, e.g.
// one stuck in a loop, is detached by closing the connection instead: the
// engine then carries on without the debugger.
func (s *Session) Detach() error {
	_, err := s.sendCommand("detach", nil, nil)
	if errors.Is(err, ErrRunning) {
		_ = s.conn.Close()
		<-s.closed
		return nil
	}
	return err
}

// GetStack returns the call stack, with local paths.
func (s *Session) GetStack() ([]StackFrame, error) {
	resp, err := s.sendCommand("stack_get", nil, nil)
	if err != nil {
		return nil, err
	}
	for i := range resp.Stack {
		resp.Stack[i].Filename = s.pathMap().localPath(resp.Stack[i].Filename)
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
	p, err := s.EvalProperty(code, 0)
	if err != nil || p == nil {
		return "", err
	}
	if p.Type == "array" || p.Type == "object" {
		return formatPropertyValue(*p), nil
	}
	return p.DecodedValue()
}

// EvalProperty evaluates an expression and returns the result as a
// property, with children for arrays and objects (page selects which).
// It returns nil for expressions without a value.
func (s *Session) EvalProperty(code string, page int) (*Property, error) {
	var args []string
	if page > 0 {
		args = []string{"-p", strconv.Itoa(page)}
	}
	resp, err := s.sendCommand("eval", args, []byte(code))
	if err != nil || len(resp.Properties) == 0 {
		return nil, err
	}
	return &resp.Properties[0], nil
}

// PropertyOptions selects where a property is looked up and how much of
// it the engine returns.
type PropertyOptions struct {
	Depth   int // stack depth: 0 is the current frame
	Context int // context id (see ContextNames): 0 is Locals
	Page    int // page of children, from 0
	MaxData int // bytes of a value to return; 0 uses the max_data feature
}

func (o PropertyOptions) args(fullname string) []string {
	args := []string{"-n", fullname, "-d", strconv.Itoa(o.Depth), "-c", strconv.Itoa(o.Context)}
	if o.Page > 0 {
		args = append(args, "-p", strconv.Itoa(o.Page))
	}
	if o.MaxData > 0 {
		args = append(args, "-m", strconv.Itoa(min(o.MaxData, MaxValueBytes)))
	}
	return args
}

// GetProperty returns a variable, or a path into one such as $user["name"]
// or $obj->cache, with one page of its children down to the max_depth
// feature. String values longer than max_data are truncated; see
// Property.Truncated and GetPropertyValue.
func (s *Session) GetProperty(fullname string, opts PropertyOptions) (*Property, error) {
	resp, err := s.sendCommand("property_get", opts.args(fullname), nil)
	if err != nil {
		return nil, err
	}
	if len(resp.Properties) == 0 {
		return nil, fmt.Errorf("property_get %s: no property in response", fullname)
	}
	return &resp.Properties[0], nil
}

// GetPropertyValue returns a property's value and the length of all of
// it. With opts.MaxData 0 it returns the whole value, however long;
// otherwise only its first MaxData bytes, so a part of a long value can be
// read without sending all of it. Page is ignored.
func (s *Session) GetPropertyValue(fullname string, opts PropertyOptions) (value string, size int, err error) {
	args := []string{"-n", fullname, "-d", strconv.Itoa(opts.Depth), "-c", strconv.Itoa(opts.Context),
		"-m", strconv.Itoa(opts.MaxData)}
	resp, err := s.sendCommand("property_value", args, nil)
	if err != nil {
		return "", 0, err
	}
	if value, err = decodeEncoded(resp.Encoding, resp.Value); err != nil {
		return "", 0, err
	}
	// The engine sends the whole length; one that does not at least sent
	// this much.
	return value, max(resp.Size, len(value)), nil
}

// SetProperty assigns value, a PHP expression, to a variable or a path
// into one. Page and MaxData are ignored.
func (s *Session) SetProperty(fullname, value string, opts PropertyOptions) error {
	args := []string{"-n", fullname, "-d", strconv.Itoa(opts.Depth), "-c", strconv.Itoa(opts.Context)}
	resp, err := s.sendCommand("property_set", args, []byte(value))
	if err != nil {
		return err
	}
	if resp.Success == "0" {
		return fmt.Errorf("property_set %s: engine reported failure", fullname)
	}
	return nil
}

// GetContextProperties returns the variables of a context at a stack depth
// as properties, with children down to the max_depth feature.
func (s *Session) GetContextProperties(depth, context int) ([]Property, error) {
	args := []string{"-d", strconv.Itoa(depth), "-c", strconv.Itoa(context)}
	resp, err := s.sendCommand("context_get", args, nil)
	if err != nil {
		return nil, err
	}
	return resp.Properties, nil
}

// ContextNames returns the variable contexts available at a stack depth,
// such as Locals and Superglobals.
func (s *Session) ContextNames(depth int) ([]ContextName, error) {
	resp, err := s.sendCommand("context_names", []string{"-d", strconv.Itoa(depth)}, nil)
	if err != nil {
		return nil, err
	}
	return resp.Contexts, nil
}

// StackDepth returns the number of frames on the call stack.
func (s *Session) StackDepth() (int, error) {
	resp, err := s.sendCommand("stack_depth", nil, nil)
	if err != nil {
		return 0, err
	}
	return resp.Depth, nil
}

// GetStackFrame returns the frame at a stack depth: 0 is the current one.
func (s *Session) GetStackFrame(depth int) (StackFrame, error) {
	resp, err := s.sendCommand("stack_get", []string{"-d", strconv.Itoa(depth)}, nil)
	if err != nil {
		return StackFrame{}, err
	}
	if len(resp.Stack) == 0 {
		return StackFrame{}, fmt.Errorf("stack_get: no frame at depth %d", depth)
	}
	frame := resp.Stack[0]
	frame.Filename = s.pathMap().localPath(frame.Filename)
	return frame, nil
}

// TypeMap returns how the engine's types map to DBGp and XML Schema types.
func (s *Session) TypeMap() ([]TypeMapEntry, error) {
	resp, err := s.sendCommand("typemap_get", nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.TypeMap, nil
}

// GetSource returns source code of a local path
func (s *Session) GetSource(file string, begin, end int) (string, error) {
	args := []string{"-f", s.pathMap().engineURI(file)}
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
