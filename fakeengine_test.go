package dbgp

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEngine is a scripted stand-in for Xdebug. It connects to the client
// under test, sends an init packet and answers commands using handlers.
// Commands are parsed the way Xdebug parses them, so malformed commands get
// the same error replies Xdebug sends.
type fakeEngine struct {
	t    *testing.T
	conn net.Conn

	mu       sync.Mutex
	received []fakeCommand
	handlers map[string]fakeHandler

	writeMu sync.Mutex
	done    chan struct{}
}

// fakeCommand is one command as received by the engine.
type fakeCommand struct {
	Raw     string            // bytes between NULs, as received
	Name    string            // command name
	Args    map[string]string // option letter -> value, e.g. "i" -> "3"
	Data    string            // base64-decoded data after "--"
	HasData bool
	Err     string // set when Xdebug would reject the command
}

// fakeHandler returns the packets to send in reply to cmd (nil sends nothing).
type fakeHandler func(e *fakeEngine, cmd fakeCommand) []string

// fixture returns a recorded engine packet from testdata/dbgp.
func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name+".xml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(data)
}

// reply returns a handler that answers with the named fixture, its
// transaction_id rewritten to match the command.
func reply(name string) fakeHandler {
	return func(e *fakeEngine, cmd fakeCommand) []string {
		return []string{e.withTransaction(e.fixture(name), cmd)}
	}
}

// fixture is like the package-level fixture, for use in handlers: they run
// on the engine goroutine, where t.Fatal is not allowed.
func (e *fakeEngine) fixture(name string) string {
	data, err := os.ReadFile(filepath.Join(fixtureDir, name+".xml"))
	if err != nil {
		e.t.Errorf("read fixture: %v", err)
	}
	return string(data)
}

// withTransaction rewrites a packet's transaction_id to the command's -i.
func (e *fakeEngine) withTransaction(packet string, cmd fakeCommand) string {
	return transactionIDAttr.ReplaceAllString(packet, fmt.Sprintf(`transaction_id="%s"`, cmd.Args["i"]))
}

// errorPacket builds an Xdebug-style error response.
func errorPacket(cmd fakeCommand, code int, message string) string {
	txn := ""
	if id, ok := cmd.Args["i"]; ok {
		txn = fmt.Sprintf(` transaction_id="%s"`, id)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="iso-8859-1"?>`+"\n"+
		`<response xmlns="urn:debugger_protocol_v1" xmlns:xdebug="https://xdebug.org/dbgp/xdebug" command="%s"%s>`+
		`<error code="%d"><message><![CDATA[%s]]></message></error></response>`, cmd.Name, txn, code, message)
}

// parseFakeCommand parses a command like Xdebug's xdebug_dbgp_parse_cmd:
// "name -a value -b \"quoted value\" -- base64data". Everything after "--"
// is data; every command must carry -i.
func parseFakeCommand(raw string) fakeCommand {
	cmd := fakeCommand{Raw: raw, Args: map[string]string{}}
	name, rest, _ := strings.Cut(raw, " ")
	cmd.Name = name
	for rest != "" {
		if !strings.HasPrefix(rest, "-") || len(rest) < 2 {
			cmd.Err = "parse error"
			return cmd
		}
		if strings.HasPrefix(rest, "--") {
			encoded := strings.TrimPrefix(strings.TrimPrefix(rest, "--"), " ")
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				cmd.Err = "invalid or missing options"
				return cmd
			}
			cmd.Data, cmd.HasData = string(decoded), true
			break
		}
		opt := rest[1:2]
		rest = strings.TrimPrefix(rest[2:], " ")
		var value string
		if strings.HasPrefix(rest, `"`) {
			// Quoted values are unescaped like the engine's stripcslashes.
			var b strings.Builder
			i := 1
			for ; i < len(rest) && rest[i] != '"'; i++ {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
					if r, ok := cEscapes[rest[i]]; ok {
						b.WriteByte(r)
						continue
					}
				}
				b.WriteByte(rest[i])
			}
			if i >= len(rest) {
				cmd.Err = "parse error"
				return cmd
			}
			value, rest = b.String(), rest[i+1:]
		} else {
			value, rest, _ = strings.Cut(rest, " ")
		}
		cmd.Args[opt] = value
		rest = strings.TrimLeft(rest, " ")
	}
	if _, ok := cmd.Args["i"]; !ok {
		cmd.Err = "invalid or missing options"
	}
	return cmd
}

// cEscapes are the C escapes stripcslashes understands (octal/hex omitted).
var cEscapes = map[byte]byte{'n': '\n', 't': '\t', 'r': '\r', 'a': '\a', 'v': '\v', 'b': '\b', 'f': '\f'}

// startFakeSession connects a fake engine to a new server and returns the
// session once the server has set it up. Setup commands (feature_set,
// stdout) are cleared from the engine's record.
func startFakeSession(t *testing.T, handlers map[string]fakeHandler) (*Session, *fakeEngine) {
	t.Helper()
	srv := newTestServer(t, Config{})
	e := dialFakeEngine(t, srv, fixture(t, "init"), handlers)
	sess := waitSession(t, srv)
	e.ClearReceived()
	return sess, e
}

// dialFakeEngine connects a fake engine to srv and sends its init packet.
func dialFakeEngine(t *testing.T, srv *Server, init string, handlers map[string]fakeHandler) *fakeEngine {
	t.Helper()
	conn := dialServer(t, srv)
	e := &fakeEngine{t: t, conn: conn, handlers: handlers, done: make(chan struct{})}
	t.Cleanup(e.Close)
	e.Send(init)
	go e.serve()
	return e
}

// waitSession waits up to 5s for the next session on srv.
func waitSession(t *testing.T, srv *Server) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := srv.WaitForSession(ctx)
	if err != nil {
		t.Fatalf("WaitForSession: %v", err)
	}
	return sess
}

// serve reads NUL-terminated commands as they arrive, recording each one,
// and answers them in order on a separate goroutine, so tests can see
// commands the client sends before an earlier one is answered.
func (e *fakeEngine) serve() {
	queue := make(chan fakeCommand, 100)
	go e.answer(queue)
	defer close(e.done)
	defer close(queue)
	r := bufio.NewReader(e.conn)
	for {
		raw, err := r.ReadString(0)
		if err != nil {
			return
		}
		cmd := parseFakeCommand(strings.TrimSuffix(raw, "\x00"))
		e.mu.Lock()
		e.received = append(e.received, cmd)
		e.mu.Unlock()
		queue <- cmd
	}
}

// answer runs the handlers for queued commands, one at a time.
func (e *fakeEngine) answer(queue <-chan fakeCommand) {
	for cmd := range queue {
		e.mu.Lock()
		handler, ok := e.handlers[cmd.Name]
		e.mu.Unlock()

		var packets []string
		switch {
		case cmd.Err == "parse error":
			packets = []string{errorPacket(cmd, 1, cmd.Err)}
		case cmd.Err != "":
			packets = []string{errorPacket(cmd, 3, cmd.Err)}
		case !ok:
			packets = []string{errorPacket(cmd, 4, "unimplemented command")}
		default:
			packets = handler(e, cmd)
		}
		for _, p := range packets {
			e.Send(p)
		}
	}
}

// Send writes one engine packet: length NUL xml NUL.
func (e *fakeEngine) Send(packet string) {
	e.SendRaw(fmt.Sprintf("%d\x00%s\x00", len(packet), packet))
}

// SendRaw writes bytes to the client unmodified.
func (e *fakeEngine) SendRaw(raw string) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	_, _ = e.conn.Write([]byte(raw))
}

// Handle installs or replaces the handler for a command.
func (e *fakeEngine) Handle(name string, h fakeHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers[name] = h
}

// ClearReceived forgets the commands received so far.
func (e *fakeEngine) ClearReceived() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.received = nil
}

// Received returns the commands received so far.
func (e *fakeEngine) Received() []fakeCommand {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]fakeCommand(nil), e.received...)
}

// LastCommand returns the most recent command with the given name.
func (e *fakeEngine) LastCommand(name string) fakeCommand {
	e.t.Helper()
	cmds := e.Received()
	for i := len(cmds) - 1; i >= 0; i-- {
		if cmds[i].Name == name {
			return cmds[i]
		}
	}
	e.t.Fatalf("engine never received %q; got %v", name, cmds)
	return fakeCommand{}
}

// Close drops the connection, as Xdebug does when the script ends.
func (e *fakeEngine) Close() {
	_ = e.conn.Close()
}

// WaitClosed waits for the client side to close the connection.
func (e *fakeEngine) WaitClosed(timeout time.Duration) bool {
	select {
	case <-e.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// dialServer opens a raw connection to srv, closed at cleanup.
func dialServer(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// newTestServer starts a server on a free local port, closed at cleanup.
func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	srv, err := Listen(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// standardHandlers answers every command the client issues with the
// matching recorded fixture.
func standardHandlers() map[string]fakeHandler {
	return map[string]fakeHandler{
		"status":            reply("status_break"),
		"feature_get":       reply("feature_get"),
		"feature_set":       reply("feature_set"),
		"breakpoint_set":    reply("breakpoint_set_line"),
		"breakpoint_remove": reply("breakpoint_remove"),
		"breakpoint_list":   reply("breakpoint_list"),
		"run":               reply("run_break"),
		"step_into":         reply("step_into"),
		"step_over":         reply("step_over"),
		"step_out":          reply("step_out"),
		"stop":              reply("stop"),
		"stdout":            reply("stdout"),
		"property_get":      reply("property_get_object"),
		"property_value":    reply("property_value"),
		"property_set":      reply("property_set"),
		"context_names":     reply("context_names"),
		"stack_depth":       reply("stack_depth"),
		"typemap_get":       reply("typemap_get"),
		"detach":            reply("detach"),
		"stack_get":         reply("stack_get"),
		"context_get":       reply("context_get_locals"),
		"eval":              reply("eval"),
		"source":            reply("source"),
	}
}
