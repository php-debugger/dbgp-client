package dbgp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAddr is where PHP Debugger and Xdebug 3 connect by default:
// localhost, which may resolve to ::1 or 127.0.0.1, so the server listens
// on both.
const DefaultAddr = "localhost:9003"

// ErrServerClosed is returned by WaitForSession after Close.
var ErrServerClosed = errors.New("server closed")

// StdoutMode selects what happens to a session's program output.
type StdoutMode int

const (
	// StdoutCopy sends output to the IDE and still prints it as usual.
	StdoutCopy StdoutMode = iota
	// StdoutRedirect sends output to the IDE only.
	StdoutRedirect
	// StdoutDisabled leaves output alone; Session.Output stays empty.
	StdoutDisabled
)

// Config configures a Server.
type Config struct {
	// Addr is the address to listen on. Defaults to DefaultAddr. A host
	// name listens on every address it resolves to; use "0.0.0.0:9003" to
	// accept engines in containers or on other hosts.
	Addr string
	// IDEKey, when set, accepts only sessions with this idekey. Others are
	// detached, so their scripts run on without debugging.
	IDEKey string
	// Stdout selects whether program output is captured. Default: StdoutCopy.
	Stdout StdoutMode
	// Features are set with feature_set on every new session, e.g.
	// "max_depth": "2". Rejected ones are reported by Session.SetupError.
	Features map[string]string
	// InitTimeout bounds how long a new connection may take to send its
	// init packet. Default: 10s.
	InitTimeout time.Duration
	// PathMap maps local directories to the engine's, for PHP running in a
	// container or on a remote server. Sessions take and return local paths.
	PathMap []PathMapping
}

// Breakpoint is a breakpoint the server applies to every session, including
// sessions that connect later.
type Breakpoint struct {
	ID        int    // assigned by AddBreakpoint
	Type      string // line (default), conditional, call, return or exception
	File      string // line and conditional: local path (see Config.PathMap)
	Line      int    // line and conditional
	Condition string // conditional: PHP expression
	Function  string // call and return
	Exception string // exception: class name
}

func (bp Breakpoint) typeName() string {
	if bp.Type == "" {
		return BreakpointLine
	}
	return bp.Type
}

// Server accepts engine connections and manages their sessions. It only
// accepts them while listening: otherwise its port is closed, so the
// engine's connection is refused at once and the script runs undebugged.
type Server struct {
	cfg Config

	// pathsMu guards paths, which is replaced, never changed in place.
	pathsMu sync.RWMutex
	paths   PathMap

	mu          sync.Mutex
	addr        string         // where to listen; the bound address after the first start
	listeners   []net.Listener // empty while not listening
	acceptDone  []chan struct{}
	warnings    []string // addresses the last StartListening could not use
	closed      bool
	handshakes  map[net.Conn]struct{}
	nextID      int
	sessions    []*Session
	queue       []*Session // not yet returned by WaitForSession
	arrived     chan struct{}
	nextBPID    int
	breakpoints []Breakpoint
}

// Listen creates a server and starts listening for engine connections.
func Listen(cfg Config) (*Server, error) {
	s, err := NewServer(cfg)
	if err != nil {
		return nil, err
	}
	if err := s.StartListening(); err != nil {
		return nil, err
	}
	return s, nil
}

// NewServer creates a server that is not listening yet; see StartListening.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.InitTimeout <= 0 {
		cfg.InitTimeout = 10 * time.Second
	}
	paths, err := newPathMap(cfg.PathMap)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:        cfg,
		paths:      paths,
		addr:       cfg.Addr,
		handshakes: map[net.Conn]struct{}{},
		arrived:    make(chan struct{}),
	}, nil
}

// StartListening starts accepting engine connections. It does nothing if
// the server is already listening. After the first start the server keeps
// its port, so restarting reuses it even when Config.Addr asked for port 0.
func (s *Server) StartListening() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServerClosed
	}
	if len(s.listeners) > 0 {
		return nil
	}
	host, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	hosts, err := listenHosts(host)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}

	var listeners []net.Listener
	var failures []string
	for _, h := range hosts {
		listener, err := net.Listen("tcp", net.JoinHostPort(h, port))
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if port == "0" {
			// The other addresses take the same port.
			_, port, _ = net.SplitHostPort(listener.Addr().String())
		}
		listeners = append(listeners, listener)
	}
	if len(listeners) == 0 {
		return fmt.Errorf("listen on %s: %s", s.addr, strings.Join(failures, "; "))
	}

	if len(hosts) == 1 {
		s.addr = listeners[0].Addr().String()
	} else {
		s.addr = net.JoinHostPort(host, port)
	}
	s.listeners, s.warnings, s.acceptDone = listeners, failures, nil
	for _, listener := range listeners {
		done := make(chan struct{})
		s.acceptDone = append(s.acceptDone, done)
		go s.acceptLoop(listener, done)
	}
	return nil
}

// listenHosts returns the hosts to listen on for a configured host: an IP
// address or empty (all addresses) as it is, a name as every address it
// resolves to, except link-local ones.
func listenHosts(host string) ([]string, error) {
	if host == "" || net.ParseIP(host) != nil {
		return []string{host}, nil
	}
	addrs, err := net.DefaultResolver.LookupHost(context.Background(), host)
	if err != nil {
		return nil, err
	}
	var hosts []string
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && !ip.IsLinkLocalUnicast() {
			hosts = append(hosts, a)
		}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%s has no usable address", host)
	}
	return hosts, nil
}

// ListenWarnings reports addresses the last StartListening could not
// listen on, although it listened on others: for localhost, typically
// another program holding [::1]:PORT, which PHP may then reach instead.
func (s *Server) ListenWarnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.warnings...)
}

// Addrs returns the addresses the server is listening on.
func (s *Server) Addrs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var addrs []string
	for _, l := range s.listeners {
		addrs = append(addrs, l.Addr().String())
	}
	return addrs
}

// StopListening stops accepting engine connections: new ones are refused.
// Sessions already connected are not affected.
func (s *Server) StopListening() error {
	s.mu.Lock()
	listeners, done := s.listeners, s.acceptDone
	s.listeners, s.acceptDone = nil, nil
	s.mu.Unlock()
	var errs []error
	for _, l := range listeners {
		errs = append(errs, l.Close())
	}
	for _, d := range done {
		<-d
	}
	return errors.Join(errs...)
}

// Listening reports whether the server is accepting engine connections.
func (s *Server) Listening() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.listeners) > 0
}

// Addr returns the address the server listens on, as configured but with
// the port it got once it has listened (a host name stays a name; see Addrs).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Port returns the port the server listens on.
func (s *Server) Port() int {
	_, port, _ := net.SplitHostPort(s.Addr())
	p, _ := strconv.Atoi(port)
	return p
}

func (s *Server) acceptLoop(listener net.Listener, done chan struct{}) {
	defer close(done)
	for {
		conn, err := listener.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			continue
		}
		s.handshakes[conn] = struct{}{}
		s.mu.Unlock()
		go s.handshake(conn)
	}
}

// handshake reads a new connection's init packet, configures the session
// and applies breakpoints before making it available. The engine waits in
// the starting state until a session is continued.
func (s *Server) handshake(conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.handshakes, conn)
		s.mu.Unlock()
	}()

	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.InitTimeout))
	reader := bufio.NewReader(conn)
	data, err := readPacket(reader)
	if err != nil {
		_ = conn.Close()
		return
	}
	init, err := ParseInit(data)
	if err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	if s.cfg.IDEKey != "" && init.IDEKey != s.cfg.IDEKey {
		// Not ours: let the script run on without the debugger.
		_, _ = conn.Write([]byte("detach -i 1\x00"))
		_ = conn.Close()
		return
	}

	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()

	sess := newSession(id, s, conn, reader, init)
	sess.setup(s.cfg, s.pathMap())
	// Failed breakpoints are retried, and reported, by the first Continue.
	_ = sess.syncBreakpoints()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = sess.Close()
		return
	}
	s.sessions = append(s.sessions, sess)
	s.queue = append(s.queue, sess)
	s.pruneSessions()
	close(s.arrived)
	s.arrived = make(chan struct{})
}

// setup enables notifications and output capture, then applies cfg.Features.
// Only commands every engine implements are sent here: Xdebug and PHP
// Debugger resume the script after an unimplemented command.
func (sess *Session) setup(cfg Config, paths PathMap) {
	// Optional: older engines may not support these.
	_ = sess.FeatureSet("notify_ok", "1")
	_ = sess.FeatureSet("resolved_breakpoints", "1")
	// Before cfg.Features, which may lower max_data below the probe's reply.
	sess.detectEngineMapping(paths)

	names := make([]string, 0, len(cfg.Features))
	for name := range cfg.Features {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		if err := sess.FeatureSet(name, cfg.Features[name]); err != nil {
			errs = append(errs, fmt.Errorf("feature %s: %w", name, err))
		}
	}

	switch cfg.Stdout {
	case StdoutCopy:
		_, _ = sess.sendCommand("stdout", []string{"-c", "1"}, nil)
	case StdoutRedirect:
		_, _ = sess.sendCommand("stdout", []string{"-c", "2"}, nil)
	}

	sess.mu.Lock()
	sess.setupErr = errors.Join(errs...)
	sess.mu.Unlock()
}

// pathMap returns the current path map.
func (s *Server) pathMap() PathMap {
	s.pathsMu.RLock()
	defer s.pathsMu.RUnlock()
	return s.paths
}

// PathMap returns the current path mappings.
func (s *Server) PathMap() []PathMapping {
	return append([]PathMapping(nil), s.pathMap()...)
}

// AddPathMapping adds a mapping, or replaces the one for the same local
// directory. It applies to every session at once: paths from the engine are
// translated with it from now on, and breakpoints already set are set again
// with their new paths, now in stopped sessions and on the next Continue in
// running ones. The returned error lists sessions that rejected a breakpoint.
func (s *Server) AddPathMapping(m PathMapping) error {
	added, err := newPathMap([]PathMapping{m})
	if err != nil {
		return err
	}
	m = added[0]
	s.pathsMu.Lock()
	paths := make(PathMap, 0, len(s.paths)+1)
	for _, old := range s.paths {
		if old.Local != m.Local {
			paths = append(paths, old)
		}
	}
	s.paths = append(paths, m)
	s.pathsMu.Unlock()

	for _, sess := range s.Sessions() {
		sess.resync.Store(true)
	}
	return s.syncStoppedSessions()
}

// pruneSessions forgets the oldest ended sessions beyond maxEndedSessions,
// with their output, so a server debugging many requests does not keep
// them all. Sessions WaitForSession has not returned yet are kept.
func (s *Server) pruneSessions() {
	ended := 0
	for _, sess := range s.sessions {
		if sess.ended() {
			ended++
		}
	}
	if ended <= maxEndedSessions {
		return
	}
	kept := s.sessions[:0]
	for _, sess := range s.sessions {
		if ended > maxEndedSessions && sess.ended() && !slices.Contains(s.queue, sess) {
			ended--
			continue
		}
		kept = append(kept, sess)
	}
	clear(s.sessions[len(kept):])
	s.sessions = kept
}

// Sessions returns the sessions in connection order, including the most
// recent ended ones.
func (s *Server) Sessions() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Session(nil), s.sessions...)
}

// Session returns the session with the given id, or nil.
func (s *Server) Session(id int) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.id == id {
			return sess
		}
	}
	return nil
}

// WaitForSession returns the oldest session not yet returned by
// WaitForSession, waiting for one to connect if needed.
func (s *Server) WaitForSession(ctx context.Context) (*Session, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			sess := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return sess, nil
		}
		if s.closed {
			s.mu.Unlock()
			return nil, ErrServerClosed
		}
		arrived := s.arrived
		s.mu.Unlock()

		select {
		case <-arrived:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// AddBreakpoint adds a breakpoint to every session: now for sessions that
// are stopped, and on their next Continue for sessions that are running.
// The returned error lists sessions that rejected it.
func (s *Server) AddBreakpoint(bp Breakpoint) (Breakpoint, error) {
	if err := bp.validate(); err != nil {
		return Breakpoint{}, err
	}
	s.mu.Lock()
	s.nextBPID++
	bp.ID = s.nextBPID
	s.breakpoints = append(s.breakpoints, bp)
	s.mu.Unlock()
	return bp, s.syncStoppedSessions()
}

// RemoveBreakpoint removes a breakpoint from every session.
func (s *Server) RemoveBreakpoint(id int) error {
	s.mu.Lock()
	found := false
	for i, bp := range s.breakpoints {
		if bp.ID == id {
			s.breakpoints = append(s.breakpoints[:i:i], s.breakpoints[i+1:]...)
			found = true
			break
		}
	}
	s.mu.Unlock()
	if !found {
		return fmt.Errorf("no breakpoint %d", id)
	}
	return s.syncStoppedSessions()
}

// Breakpoints returns the server's breakpoints.
func (s *Server) Breakpoints() []Breakpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Breakpoint(nil), s.breakpoints...)
}

// syncStoppedSessions applies the breakpoints to sessions that can take
// commands now; running ones sync on their next Continue.
func (s *Server) syncStoppedSessions() error {
	var errs []error
	for _, sess := range s.Sessions() {
		if sess.State().Status == StatusRunning {
			continue
		}
		if err := sess.syncBreakpoints(); err != nil {
			errs = append(errs, fmt.Errorf("session %d: %w", sess.id, err))
		}
	}
	return errors.Join(errs...)
}

func (bp Breakpoint) validate() error {
	switch bp.typeName() {
	case BreakpointLine, BreakpointConditional:
		if bp.File == "" || bp.Line <= 0 {
			return fmt.Errorf("%s breakpoint needs a file and line", bp.typeName())
		}
		if bp.typeName() == BreakpointConditional && bp.Condition == "" {
			return fmt.Errorf("conditional breakpoint needs a condition")
		}
	case BreakpointCall, BreakpointReturn:
		if bp.Function == "" {
			return fmt.Errorf("%s breakpoint needs a function", bp.Type)
		}
	case BreakpointException:
		if bp.Exception == "" {
			return fmt.Errorf("exception breakpoint needs an exception class")
		}
	default:
		return fmt.Errorf("unsupported breakpoint type %q", bp.Type)
	}
	return nil
}

// Close stops listening and closes every session.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.arrived)
	s.arrived = make(chan struct{})
	for conn := range s.handshakes {
		_ = conn.Close()
	}
	sessions := append([]*Session(nil), s.sessions...)
	s.mu.Unlock()

	err := s.StopListening()
	for _, sess := range sessions {
		_ = sess.Close()
	}
	return err
}
