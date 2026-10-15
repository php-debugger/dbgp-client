package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The background MCP server, over HTTP: dbgp mcp start starts it and
// returns, dbgp mcp stop stops it, and dbgp mcp started by an agent passes
// the agent on to it. Its state file records its process and URL, so they
// find it; there is one per user.

// serveCommand runs the background server in this process. dbgp mcp start
// runs it; it is not meant to be typed.
const serveCommand = "serve"

// defaultHTTPAddr is where agents connect: on this machine only, next to
// DBGp's 9003.
const defaultHTTPAddr = "127.0.0.1:9004"

// How long the commands wait for the server to start or stop.
const (
	startTimeout = 10 * time.Second
	stopTimeout  = 5 * time.Second
)

// daemonState is what the state file records about a running server.
type daemonState struct {
	PID int    `json:"pid"`
	URL string `json:"url"`
}

// stateDir is where the state file and the background server's log are.
func stateDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("no directory for dbgp's state: %w", err)
	}
	return filepath.Join(dir, "dbgp"), nil
}

func statePath() (string, error) {
	dir, err := stateDir()
	return filepath.Join(dir, "mcp.json"), err
}

func logPath() (string, error) {
	dir, err := stateDir()
	return filepath.Join(dir, "mcp.log"), err
}

// readState returns the recorded server, or nil when there is none.
func readState() (*daemonState, error) {
	path, err := statePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st daemonState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &st, nil
}

func writeState(st daemonState) error {
	path, err := statePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// removeState removes the state file if it still records process pid.
func removeState(pid int) {
	if st, err := readState(); err == nil && st != nil && st.PID == pid {
		if path, err := statePath(); err == nil {
			_ = os.Remove(path)
		}
	}
}

// running returns the recorded server if it is answering, removing a state
// file left behind by one that is not.
func running() (*daemonState, error) {
	st, err := readState()
	if err != nil || st == nil {
		return nil, err
	}
	if !answering(st.URL) {
		removeState(st.PID)
		return nil, nil
	}
	return st, nil
}

// answering reports whether something accepts connections at rawURL.
func answering(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// startDaemon starts the server in the background, running this program
// with serveCommand and args, and waits for it to answer.
func startDaemon(args []string) int {
	if st, err := running(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	} else if st != nil {
		fmt.Printf("dbgp MCP server already running at %s (pid %d)\n"+
			"To start it with other options, stop it first with: dbgp mcp stop\n", st.URL, st.PID)
		return 0
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logFile, err := openLog()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmd := exec.Command(exe, append([]string{"mcp", serveCommand}, args...)...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detach(cmd)
	err = cmd.Start()
	_ = logFile.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	deadline := time.After(startTimeout)
	for {
		if st, _ := readState(); st != nil && st.PID == cmd.Process.Pid && answering(st.URL) {
			fmt.Printf("dbgp MCP server running at %s (pid %d)\n"+
				"Agents that start dbgp mcp now use it. Stop it with: dbgp mcp stop\n", st.URL, st.PID)
			return 0
		}
		select {
		case <-exited:
			fmt.Fprintf(os.Stderr, "dbgp MCP server did not start:\n%s", readLog())
			return 1
		case <-deadline:
			_ = cmd.Process.Kill()
			fmt.Fprintf(os.Stderr, "dbgp MCP server did not start within %v:\n%s", startTimeout, readLog())
			return 1
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// openLog creates the background server's log, replacing the last one.
func openLog() (*os.File, error) {
	path, err := logPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
}

func readLog() string {
	path, err := logPath()
	if err != nil {
		return ""
	}
	data, _ := os.ReadFile(path)
	return string(data)
}

// stopDaemon stops the background server and waits for it to go.
func stopDaemon() int {
	st, err := running()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if st == nil {
		fmt.Println("dbgp MCP server is not running")
		return 0
	}
	proc, err := os.FindProcess(st.PID)
	if err == nil {
		err = terminate(proc)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot stop the dbgp MCP server (pid %d): %v\n", st.PID, err)
		return 1
	}
	for deadline := time.Now().Add(stopTimeout); answering(st.URL); {
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "dbgp MCP server (pid %d) did not stop within %v\n", st.PID, stopTimeout)
			return 1
		}
		time.Sleep(50 * time.Millisecond)
	}
	// A server that was killed could not remove it itself.
	removeState(st.PID)
	fmt.Printf("dbgp MCP server stopped (pid %d)\n", st.PID)
	return 0
}

// daemonStatus shows whether the background server is running.
func daemonStatus() int {
	st, err := running()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if st == nil {
		fmt.Println("dbgp MCP server is not running")
		return 1
	}
	fmt.Printf("dbgp MCP server running at %s (pid %d)\n", st.URL, st.PID)
	return 0
}

// bridge passes MCP messages between stdin and stdout and the background
// server at url, until either side closes.
func bridge(ctx context.Context, url string) error {
	local, err := (&mcp.StdioTransport{}).Connect(ctx)
	if err != nil {
		return err
	}
	defer local.Close()
	remote, err := (&mcp.StreamableClientTransport{Endpoint: url}).Connect(ctx)
	if err != nil {
		return err
	}
	defer remote.Close()

	done := make(chan error, 2)
	pass := func(from, to mcp.Connection) {
		for {
			msg, err := from.Read(ctx)
			if err == nil {
				err = to.Write(ctx, msg)
			}
			if err != nil {
				done <- err
				return
			}
		}
	}
	go pass(local, remote)
	go pass(remote, local)
	// The agent closing stdin is the usual end.
	if err := <-done; !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// serveHTTP serves MCP over HTTP at addr until ctx is done, recording the
// server in the state file meanwhile.
func serveHTTP(ctx context.Context, server *mcp.Server, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	st := daemonState{PID: os.Getpid(), URL: "http://" + ln.Addr().String() + "/mcp"}
	if err := writeState(st); err != nil {
		_ = ln.Close()
		return err
	}
	defer removeState(st.PID)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{CrossOriginProtection: http.NewCrossOriginProtection()})
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		// Tools may be waiting for PHP for minutes: do not wait for them.
		_ = hs.Close()
	}()
	fmt.Fprintf(os.Stderr, "dbgp MCP server listening at %s\n", st.URL)
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
