package dbgp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client manages the DBGp connection
type Client struct {
	listener net.Listener
	conn     net.Conn
	reader   *bufio.Reader
	writer   io.Writer

	mu        sync.Mutex
	writeMu   sync.Mutex
	transID   int
	responses map[int]chan *Response

	init *InitPacket

	onBreakpoint func(file string, line int, stack []StackFrame, vars []Variable)
}

// NewClient creates a new DBGp client (server that accepts PHP connections)
func NewClient(port int) (*Client, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen on port %d: %w", port, err)
	}

	return &Client{
		listener:  listener,
		responses: make(map[int]chan *Response),
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
		}
	}

	conn, err := c.listener.Accept()
	if err != nil {
		return fmt.Errorf("accept connection: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.reader = bufio.NewReader(conn)
	c.writer = conn
	c.mu.Unlock()

	// Read init packet
	initData, err := c.readPacket()
	if err != nil {
		return fmt.Errorf("read init packet: %w", err)
	}

	initPacket, err := ParseInit(initData)
	if err != nil {
		return fmt.Errorf("parse init packet: %w", err)
	}
	c.mu.Lock()
	c.init = initPacket
	c.mu.Unlock()

	// Start response reader
	go c.readLoop()

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

// readPacket reads a single DBGp packet (length + NULL + data)
func (c *Client) readPacket() ([]byte, error) {
	// Read length until NULL
	line, err := c.reader.ReadString('\x00')
	if err != nil {
		return nil, err
	}

	// Parse length
	line = strings.TrimSuffix(line, "\x00")
	length, err := strconv.Atoi(line)
	if err != nil {
		return nil, fmt.Errorf("parse packet length: %w", err)
	}

	// Read exact number of bytes
	data := make([]byte, length)
	_, err = io.ReadFull(c.reader, data)
	if err != nil {
		return nil, err
	}

	// Read trailing NULL
	b, err := c.reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if b != '\x00' {
		return nil, fmt.Errorf("expected NULL terminator, got %x", b)
	}

	return data, nil
}

// readLoop continuously reads responses and dispatches them
func (c *Client) readLoop() {
	for {
		data, err := c.readPacket()
		if err != nil {
			c.mu.Lock()
			for id, ch := range c.responses {
				ch <- &Response{
					Transaction: id,
					Error: &Error{
						Code:    -1,
						Message: "connection closed",
					},
				}
				delete(c.responses, id)
			}
			c.mu.Unlock()
			return
		}

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

		// Dispatch to waiting sender
		c.mu.Lock()
		ch, ok := c.responses[resp.Transaction]
		if ok {
			ch <- resp
			delete(c.responses, resp.Transaction)
		}
		c.mu.Unlock()
	}
}

// handleBreakpoint handles async breakpoint notifications
func (c *Client) handleBreakpoint(resp *Response) {
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

// nextTransID generates a new transaction ID
func (c *Client) nextTransID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.transID++
	return c.transID
}

// sendCommand sends a command and waits for response
func (c *Client) sendCommand(cmd string) (*Response, error) {
	c.mu.Lock()
	conn := c.conn
	writer := c.writer
	c.mu.Unlock()
	if conn == nil || writer == nil {
		return nil, fmt.Errorf("not connected")
	}

	transID := c.nextTransID()

	// Ensure transaction ID is in command
	fields := strings.Fields(cmd)
	hasTransID := false
	for i := 0; i < len(fields); i++ {
		if fields[i] == "-i" {
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("invalid command: missing transaction id after -i")
			}
			id, err := strconv.Atoi(fields[i+1])
			if err != nil {
				return nil, fmt.Errorf("invalid transaction id %q: %w", fields[i+1], err)
			}
			transID = id
			hasTransID = true
			break
		}
	}
	if !hasTransID {
		cmd = fmt.Sprintf("%s -i %d", cmd, transID)
	}

	// Create response channel
	ch := make(chan *Response, 1)
	c.mu.Lock()
	c.responses[transID] = ch
	c.mu.Unlock()

	// Send command
	packet := fmt.Sprintf("%d\x00%s\x00", len(cmd), cmd)
	c.writeMu.Lock()
	_, err := writer.Write([]byte(packet))
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.responses, transID)
		c.mu.Unlock()
		return nil, fmt.Errorf("send command: %w", err)
	}

	// Wait for response with timeout
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp, fmt.Errorf("error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp, nil
	case <-time.After(30 * time.Second):
		c.mu.Lock()
		delete(c.responses, transID)
		c.mu.Unlock()
		return nil, fmt.Errorf("timeout waiting for response")
	}
}

// SetBreakpoint sets a line breakpoint
func (c *Client) SetBreakpoint(file string, line int) (int, error) {
	uri := MakeFileURI(file)
	cmd := fmt.Sprintf("breakpoint_set -t line -f %s -n %d", uri, line)
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return 0, err
	}
	return resp.BreakpointID, nil
}

// SetConditionalBreakpoint sets a conditional breakpoint
func (c *Client) SetConditionalBreakpoint(file string, line int, condition string) (int, error) {
	uri := MakeFileURI(file)
	encoded := base64.StdEncoding.EncodeToString([]byte(condition))
	cmd := fmt.Sprintf("breakpoint_set -t conditional -f %s -n %d -- %s", uri, line, encoded)
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return 0, err
	}
	return resp.BreakpointID, nil
}

// RemoveBreakpoint removes a breakpoint
func (c *Client) RemoveBreakpoint(id int) error {
	cmd := fmt.Sprintf("breakpoint_remove -d %d", id)
	_, err := c.sendCommand(cmd)
	return err
}

// ListBreakpoints lists all breakpoints
func (c *Client) ListBreakpoints() ([]BreakpointInfo, error) {
	resp, err := c.sendCommand("breakpoint_list")
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Breakpoints []BreakpointInfo `xml:"breakpoint"`
	}
	decoder := xml.NewDecoder(bytes.NewReader([]byte("<response>" + resp.Raw + "</response>")))
	decoder.CharsetReader = charsetReader
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse breakpoint list: %w", err)
	}

	return parsed.Breakpoints, nil
}

// Run starts or continues execution
func (c *Client) Run() error {
	_, err := c.sendCommand("run")
	return err
}

// StepInto steps into the next statement
func (c *Client) StepInto() error {
	_, err := c.sendCommand("step_into")
	return err
}

// StepOver steps over the next statement
func (c *Client) StepOver() error {
	_, err := c.sendCommand("step_over")
	return err
}

// StepOut steps out of the current function
func (c *Client) StepOut() error {
	_, err := c.sendCommand("step_out")
	return err
}

// Stop stops execution
func (c *Client) Stop() error {
	_, err := c.sendCommand("stop")
	return err
}

// Detach detaches the debugger (script continues)
func (c *Client) Detach() error {
	_, err := c.sendCommand("detach")
	return err
}

// GetStack returns the call stack
func (c *Client) GetStack() ([]StackFrame, error) {
	resp, err := c.sendCommand("stack_get")
	if err != nil {
		return nil, err
	}
	return resp.Stack, nil
}

// GetContext returns variables at the given depth and context
func (c *Client) GetContext(depth int, context int) ([]Variable, error) {
	cmd := fmt.Sprintf("context_get -d %d -c %d", depth, context)
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return nil, err
	}
	return ParseVariablesFromProperties(resp.Properties), nil
}

// Eval evaluates an expression
func (c *Client) Eval(code string) (string, error) {
	encoded := base64.StdEncoding.EncodeToString([]byte(code))
	cmd := fmt.Sprintf("eval -- %s", encoded)
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return "", err
	}
	return resp.Raw, nil
}

// GetSource returns source code
func (c *Client) GetSource(file string, begin, end int) (string, error) {
	uri := MakeFileURI(file)
	cmd := fmt.Sprintf("source -f %s", uri)
	if begin > 0 {
		cmd += fmt.Sprintf(" -b %d", begin)
	}
	if end > 0 {
		cmd += fmt.Sprintf(" -e %d", end)
	}
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return "", err
	}
	return decodeResponseValue(resp)
}

// Status returns current debugger status
func (c *Client) Status() (string, error) {
	resp, err := c.sendCommand("status")
	if err != nil {
		return "", err
	}
	return resp.Status, nil
}

// FeatureGet gets a feature value
func (c *Client) FeatureGet(name string) (string, error) {
	if strings.ContainsAny(name, " \t\r\n") {
		return "", fmt.Errorf("feature name must not contain whitespace")
	}
	cmd := fmt.Sprintf("feature_get -n %s", name)
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return "", err
	}
	return decodeResponseValue(resp)
}

// FeatureSet sets a feature value
func (c *Client) FeatureSet(name, value string) error {
	if strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("feature name must not contain whitespace")
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return fmt.Errorf("feature value must not contain whitespace")
	}
	cmd := fmt.Sprintf("feature_set -n %s -v %s", name, value)
	_, err := c.sendCommand(cmd)
	return err
}

// Close closes the connection
func (c *Client) Close() error {
	var closeErr error
	c.mu.Lock()
	conn := c.conn
	listener := c.listener
	c.conn = nil
	c.reader = nil
	c.writer = nil
	c.mu.Unlock()

	if conn != nil {
		closeErr = errors.Join(closeErr, conn.Close())
	}
	if listener != nil {
		closeErr = errors.Join(closeErr, listener.Close())
	}
	return closeErr
}

func decodeResponseValue(resp *Response) (string, error) {
	value := strings.TrimSpace(resp.Value)
	if value == "" {
		value = strings.TrimSpace(resp.Raw)
	}
	if resp.Encoding == "base64" && value != "" {
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return "", fmt.Errorf("decode base64 response: %w", err)
		}
		return string(decoded), nil
	}
	return value, nil
}
