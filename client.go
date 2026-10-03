package dbgp

import (
	"bufio"
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
const commandTimeout = 30 * time.Second

var (
	errNotConnected     = errors.New("not connected")
	errConnectionClosed = errors.New("connection closed")
)

// Client manages the DBGp connection
type Client struct {
	listener net.Listener

	// cmdSlot serializes commands: at most one is in flight per connection,
	// so an error response without a transaction_id belongs to it.
	cmdSlot chan struct{}
	breakMu sync.Mutex

	mu      sync.Mutex
	conn    net.Conn
	closed  chan struct{} // closed when the read loop for conn exits
	transID int
	pending *pendingCommand

	init *InitPacket

	onBreakpoint func(file string, line int, stack []StackFrame, vars []Variable)
}

// pendingCommand is a command waiting for its response.
type pendingCommand struct {
	id int
	ch chan *Response
}

// NewClient creates a new DBGp client (server that accepts PHP connections)
func NewClient(port int) (*Client, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen on port %d: %w", port, err)
	}

	return &Client{
		listener: listener,
		cmdSlot:  make(chan struct{}, 1),
	}, nil
}

// Addr returns the address the client is listening on
func (c *Client) Addr() string {
	return c.listener.Addr().String()
}

// Port returns the port number
func (c *Client) Port() int {
	addr := c.Addr()
	_, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	return p
}

// WaitForConnection waits for PHP to connect
func (c *Client) WaitForConnection(timeout time.Duration) error {
	// Set accept deadline if timeout specified
	if timeout > 0 {
		if deadlineListener, ok := c.listener.(interface{ SetDeadline(time.Time) error }); ok {
			if err := deadlineListener.SetDeadline(time.Now().Add(timeout)); err != nil {
				return fmt.Errorf("set accept deadline: %w", err)
			}
		} else {
			return fmt.Errorf("listener does not support deadlines")
		}
	}

	conn, err := c.listener.Accept()
	if err != nil {
		return fmt.Errorf("accept connection: %w", err)
	}

	// Read init packet
	reader := bufio.NewReader(conn)
	initData, err := readPacket(reader)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("read init packet: %w", err)
	}

	initPacket, err := ParseInit(initData)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("parse init packet: %w", err)
	}

	closed := make(chan struct{})
	c.mu.Lock()
	c.conn = conn
	c.closed = closed
	c.pending = nil
	c.init = initPacket
	c.mu.Unlock()

	// Start response reader
	go c.readLoop(reader, closed)

	return nil
}

// Init returns the initialization packet from PHP
func (c *Client) Init() *InitPacket {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.init
}

// OnBreakpoint sets the callback for breakpoint hits
func (c *Client) OnBreakpoint(fn func(file string, line int, stack []StackFrame, vars []Variable)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onBreakpoint = fn
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

// readLoop reads packets from one connection and dispatches responses.
// It closes closed when the connection ends, which fails any pending and
// later commands on that connection.
func (c *Client) readLoop(reader *bufio.Reader, closed chan struct{}) {
	defer close(closed)
	for {
		data, err := readPacket(reader)
		if err != nil {
			return
		}

		// stream and notify packets are not responses; skip them
		resp, err := ParseResponse(data)
		if err != nil {
			continue
		}

		// Check if this is a breakpoint hit (async response)
		c.mu.Lock()
		onBreakpoint := c.onBreakpoint
		c.mu.Unlock()
		if resp.Status == StatusBreak && onBreakpoint != nil {
			go c.handleBreakpoint(resp)
		}

		c.dispatch(resp, closed)
	}
}

// dispatch hands a response to the pending command it answers. Errors the
// engine sends without a transaction_id (commands it could not parse) go to
// the pending command too: only one command is ever in flight.
func (c *Client) dispatch(resp *Response, closed chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pending
	if p == nil || c.closed != closed {
		return
	}
	if resp.Transaction == p.id || (resp.Transaction == 0 && resp.Error != nil) {
		c.pending = nil
		p.ch <- resp
	}
}

// handleBreakpoint handles async breakpoint notifications
func (c *Client) handleBreakpoint(resp *Response) {
	c.breakMu.Lock()
	defer c.breakMu.Unlock()

	file, line := resp.ParseMessage()

	// Get stack
	stack, _ := c.GetStack()

	// Get local variables
	vars, _ := c.GetContext(0, 0)

	c.mu.Lock()
	onBreakpoint := c.onBreakpoint
	c.mu.Unlock()
	if onBreakpoint != nil {
		onBreakpoint(file, line, stack, vars)
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

// sendCommand sends a command and waits for its response. Commands are
// serialized, so this also waits for any command already in flight.
func (c *Client) sendCommand(name string, args []string, data []byte) (*Response, error) {
	timeout := time.NewTimer(commandTimeout)
	defer timeout.Stop()

	select {
	case c.cmdSlot <- struct{}{}:
		defer func() { <-c.cmdSlot }()
	case <-timeout.C:
		return nil, fmt.Errorf("%s: timeout waiting for previous command", name)
	}

	c.mu.Lock()
	conn, closed := c.conn, c.closed
	if conn == nil {
		c.mu.Unlock()
		return nil, errNotConnected
	}
	select {
	case <-closed:
		c.mu.Unlock()
		return nil, errConnectionClosed
	default:
	}
	c.transID++
	line, err := formatCommand(name, c.transID, args, data)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	p := &pendingCommand{id: c.transID, ch: make(chan *Response, 1)}
	c.pending = p
	c.mu.Unlock()

	if _, err := conn.Write([]byte(line + "\x00")); err != nil {
		c.clearPending(p)
		return nil, fmt.Errorf("send command: %w", err)
	}

	select {
	case resp := <-p.ch:
		return responseResult(resp)
	case <-closed:
		c.clearPending(p)
		// The response may have arrived just before the connection closed.
		select {
		case resp := <-p.ch:
			return responseResult(resp)
		default:
			return nil, errConnectionClosed
		}
	case <-timeout.C:
		c.clearPending(p)
		return nil, fmt.Errorf("%s: timeout waiting for response", name)
	}
}

func (c *Client) clearPending(p *pendingCommand) {
	c.mu.Lock()
	if c.pending == p {
		c.pending = nil
	}
	c.mu.Unlock()
}

// responseResult turns an engine error response into a Go error.
func responseResult(resp *Response) (*Response, error) {
	if resp.Error != nil {
		return resp, fmt.Errorf("error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp, nil
}

// SetBreakpoint sets a line breakpoint
func (c *Client) SetBreakpoint(file string, line int) (int, error) {
	args := []string{"-t", BreakpointLine, "-f", MakeFileURI(file), "-n", strconv.Itoa(line)}
	resp, err := c.sendCommand("breakpoint_set", args, nil)
	if err != nil {
		return 0, err
	}
	return resp.BreakpointID, nil
}

// SetConditionalBreakpoint sets a conditional breakpoint
func (c *Client) SetConditionalBreakpoint(file string, line int, condition string) (int, error) {
	args := []string{"-t", BreakpointConditional, "-f", MakeFileURI(file), "-n", strconv.Itoa(line)}
	resp, err := c.sendCommand("breakpoint_set", args, []byte(condition))
	if err != nil {
		return 0, err
	}
	return resp.BreakpointID, nil
}

// RemoveBreakpoint removes a breakpoint
func (c *Client) RemoveBreakpoint(id int) error {
	_, err := c.sendCommand("breakpoint_remove", []string{"-d", strconv.Itoa(id)}, nil)
	return err
}

// ListBreakpoints lists all breakpoints
func (c *Client) ListBreakpoints() ([]BreakpointInfo, error) {
	resp, err := c.sendCommand("breakpoint_list", nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Breakpoints, nil
}

// Run starts or continues execution
func (c *Client) Run() error {
	_, err := c.sendCommand("run", nil, nil)
	return err
}

// StepInto steps into the next statement
func (c *Client) StepInto() error {
	_, err := c.sendCommand("step_into", nil, nil)
	return err
}

// StepOver steps over the next statement
func (c *Client) StepOver() error {
	_, err := c.sendCommand("step_over", nil, nil)
	return err
}

// StepOut steps out of the current function
func (c *Client) StepOut() error {
	_, err := c.sendCommand("step_out", nil, nil)
	return err
}

// Stop stops execution
func (c *Client) Stop() error {
	_, err := c.sendCommand("stop", nil, nil)
	return err
}

// Detach detaches the debugger (script continues)
func (c *Client) Detach() error {
	_, err := c.sendCommand("detach", nil, nil)
	return err
}

// GetStack returns the call stack
func (c *Client) GetStack() ([]StackFrame, error) {
	resp, err := c.sendCommand("stack_get", nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Stack, nil
}

// GetContext returns variables at the given depth and context
func (c *Client) GetContext(depth int, context int) ([]Variable, error) {
	args := []string{"-d", strconv.Itoa(depth), "-c", strconv.Itoa(context)}
	resp, err := c.sendCommand("context_get", args, nil)
	if err != nil {
		return nil, err
	}
	return ParseVariablesFromProperties(resp.Properties), nil
}

// Eval evaluates an expression and returns its value. Strings and numbers
// are returned as-is; arrays and objects in summary form, e.g. "array[3]".
func (c *Client) Eval(code string) (string, error) {
	resp, err := c.sendCommand("eval", nil, []byte(code))
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
func (c *Client) GetSource(file string, begin, end int) (string, error) {
	args := []string{"-f", MakeFileURI(file)}
	if begin > 0 {
		args = append(args, "-b", strconv.Itoa(begin))
	}
	if end > 0 {
		args = append(args, "-e", strconv.Itoa(end))
	}
	resp, err := c.sendCommand("source", args, nil)
	if err != nil {
		return "", err
	}
	return decodeResponseValue(resp)
}

// Status returns current debugger status
func (c *Client) Status() (string, error) {
	resp, err := c.sendCommand("status", nil, nil)
	if err != nil {
		return "", err
	}
	return resp.Status, nil
}

// FeatureGet gets a feature value. It fails for features the engine does
// not support.
func (c *Client) FeatureGet(name string) (string, error) {
	if strings.ContainsAny(name, " \t\r\n") {
		return "", fmt.Errorf("feature name must not contain whitespace")
	}
	resp, err := c.sendCommand("feature_get", []string{"-n", name}, nil)
	if err != nil {
		return "", err
	}
	if resp.Supported == "0" {
		return "", fmt.Errorf("feature %s is not supported", name)
	}
	return decodeResponseValue(resp)
}

// FeatureSet sets a feature value
func (c *Client) FeatureSet(name, value string) error {
	if strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("feature name must not contain whitespace")
	}
	_, err := c.sendCommand("feature_set", []string{"-n", name, "-v", value}, nil)
	return err
}

// Close closes the connection
func (c *Client) Close() error {
	var closeErr error
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()

	if conn != nil {
		closeErr = errors.Join(closeErr, conn.Close())
	}
	return errors.Join(closeErr, c.listener.Close())
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
