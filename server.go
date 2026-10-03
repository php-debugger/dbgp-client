package dbgp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
)

// DefaultAddr is where PHP Debugger and Xdebug 3 connect by default.
const DefaultAddr = "127.0.0.1:9003"

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
	// Addr is the address to listen on. Defaults to DefaultAddr; use
	// "0.0.0.0:9003" to accept engines in containers or on other hosts.
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

// Server accepts engine connections and manages their sessions.
type Server struct {
	cfg      Config
	paths    PathMap
	listener net.Listener
	done     chan struct{} // closed when the accept loop exits

	mu          sync.Mutex
	closed      bool
	handshakes  map[net.Conn]struct{}
	nextID      int
	sessions    []*Session
	queue       []*Session // not yet returned by WaitForSession
	arrived     chan struct{}
	nextBPID    int
	breakpoints []Breakpoint
}

// Listen starts a server that accepts engine connections.
func Listen(cfg Config) (*Server, error) {
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
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	s := &Server{
		cfg:        cfg,
		paths:      paths,
		listener:   listener,
		done:       make(chan struct{}),
		handshakes: map[net.Conn]struct{}{},
		arrived:    make(chan struct{}),
	}
	go s.acceptLoop()
	return s, nil
}

// Addr returns the address the server is listening on.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Port returns the port the server is listening on.
func (s *Server) Port() int {
	_, port, _ := net.SplitHostPort(s.Addr())
	p, _ := strconv.Atoi(port)
	return p
}

func (s *Server) acceptLoop() {
	defer close(s.done)
	for {
		conn, err := s.listener.Accept()
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
	sess.setup(s.cfg)
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
	close(s.arrived)
	s.arrived = make(chan struct{})
}

// setup enables notifications and output capture, then applies cfg.Features.
// Only commands every engine implements are sent here: Xdebug and PHP
// Debugger resume the script after an unimplemented command.
func (sess *Session) setup(cfg Config) {
	// Optional: older engines may not support these.
	_ = sess.FeatureSet("notify_ok", "1")
	_ = sess.FeatureSet("resolved_breakpoints", "1")

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

// Sessions returns all sessions in connection order, including ended ones.
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
		st := sess.State()
		if st.Closed || st.Status == StatusRunning {
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

// Close stops accepting connections and closes every session.
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

	err := s.listener.Close()
	for _, sess := range sessions {
		_ = sess.Close()
	}
	<-s.done
	return err
}
