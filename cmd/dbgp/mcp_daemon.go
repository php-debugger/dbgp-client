package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The background MCP server, over HTTP: dbgp mcp start starts it and
// returns, dbgp mcp stop stops it, and dbgp mcp started by an agent passes
// the agent on to it. Its state file records its process and URL, so they
// find it; there is one per user. The state file also holds a token that
// every request must carry: only the user can read the file, so other users
// on the machine cannot use the server, which runs PHP code for its agents.
// The token also proves the server is the one the file records, before
// anything is sent to it: see verify.

// serveCommand runs the background server in this process. dbgp mcp start
// runs it; it is not meant to be typed.
const serveCommand = "serve"

// The background server listens on httpHost, on this machine only: agents
// on it read the server's token from the state file, and the token is sent
// unencrypted. An IP address, not localhost, so no lookup can send it
// elsewhere. Its port defaults to defaultHTTPPort, next to PHP's 9003.
const (
	httpHost        = "127.0.0.1"
	defaultHTTPPort = 9004
)

// How long the commands wait for the server to start or stop.
const (
	startTimeout = 10 * time.Second
	stopTimeout  = 5 * time.Second
)

// daemonState is what the state file records about a running server.
type daemonState struct {
	PID int    `json:"pid"`
	URL string `json:"url"`
	// Token authenticates agents: requests send it as a bearer token.
	Token string `json:"token"`
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	// A new file, renamed into place: it is readable by the user only even
	// if an old state file was not, and readers never see half of it.
	f, err := os.CreateTemp(dir, "mcp-*.json")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// removeState removes the state file if it still records process pid.
func removeState(pid int) {
	if st, err := readState(); err == nil && st != nil && st.PID == pid {
		if path, err := statePath(); err == nil {
			_ = os.Remove(path)
		}
	}
}

// errNotOurs is returned by running when something other than the
// recorded server answers at its URL: the server died, and another
// program, perhaps another user's, took its port.
var errNotOurs = errors.New("something other than the dbgp MCP server answers")

// running returns the recorded server if it is answering, removing a state
// file left behind by one that is not. It fails with errNotOurs when
// something else answers in its place.
func running() (*daemonState, error) {
	st, err := readState()
	if err != nil || st == nil {
		return nil, err
	}
	if !answering(st.URL) {
		removeState(st.PID)
		return nil, nil
	}
	if !verify(st) {
		removeState(st.PID)
		return nil, fmt.Errorf("%w at %s, where it ran: ignoring it", errNotOurs, st.URL)
	}
	return st, nil
}

// identifyPath is where the server proves who it is; see verify.
const identifyPath = "/identify"

// identity is the server's answer to a challenge.
type identity struct {
	PID   int    `json:"pid"`
	Proof string `json:"proof"`
}

// identityProof answers challenge nonce for the server with pid: an HMAC
// keyed with the token, which only the user can read.
func identityProof(token, nonce string, pid int) string {
	mac := hmac.New(sha256.New, []byte(token))
	fmt.Fprintf(mac, "dbgp identify\x00%s\x00%d", nonce, pid)
	return hex.EncodeToString(mac.Sum(nil))
}

// verify reports whether the server at st.URL is the one st records, with
// its pid: it must answer a fresh challenge with a proof only a holder of
// st's token can give. The token itself is not sent, so a program that took
// the port of a server that died learns nothing from being asked.
func verify(st *daemonState) bool {
	u, err := url.Parse(st.URL)
	if err != nil || st.Token == "" {
		return false
	}
	nonce := rand.Text()
	u.Path, u.RawQuery = identifyPath, url.Values{"nonce": {nonce}}.Encode()
	client := &http.Client{
		Timeout: 2 * time.Second,
		// Only the recorded server may answer.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(u.String())
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var id identity
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&id) != nil {
		return false
	}
	want := identityProof(st.Token, nonce, st.PID)
	return id.PID == st.PID && hmac.Equal([]byte(id.Proof), []byte(want))
}

// identify answers verify's challenges for the server st.
func identify(st daemonState) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		nonce := req.URL.Query().Get("nonce")
		if len(nonce) < 16 || len(nonce) > 256 {
			http.Error(w, "nonce must be 16 to 256 characters", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(identity{PID: st.PID, Proof: identityProof(st.Token, nonce, st.PID)})
	})
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
	if st, err := running(); errors.Is(err, errNotOurs) {
		// Its state is gone: start a new one.
		fmt.Fprintln(os.Stderr, err)
	} else if err != nil {
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
		if st, _ := readState(); st != nil && st.PID == cmd.Process.Pid && verify(st) {
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
// server st, until either side closes.
func bridge(ctx context.Context, st *daemonState) error {
	local, err := (&mcp.StdioTransport{}).Connect(ctx)
	if err != nil {
		return err
	}
	defer local.Close()
	// Agents share the server, but each has its own selected session and
	// read positions: the id tells the server which agent a request is from.
	client, err := tokenClient(st.URL, st.Token, rand.Text())
	if err != nil {
		return err
	}
	presence, err := attachServer(ctx, client, st.URL)
	if err != nil {
		return err
	}
	defer presence.Close()
	remote, err := (&mcp.StreamableClientTransport{Endpoint: st.URL, HTTPClient: client}).Connect(ctx)
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

// attachPath is where dbgp mcp tells the server its agent is there, for as
// long as it runs: it keeps the request open, and the server takes the
// agent as gone when the request ends. The operating system ends it when
// dbgp mcp ends, even when it crashes or is killed.
const attachPath = "/attach"

// attachServer tells the server at rawURL, through client, that the agent
// client sends the id of is there, until the returned body is closed or
// ctx is done.
func attachServer(ctx context.Context, client *http.Client, rawURL string) (io.Closer, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	u.Path = attachPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("attach to the background server: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("attach to the background server: %s", resp.Status)
	}
	return resp.Body, nil
}

// attach holds each request open until the agent it is from, by its
// agentHeader, has gone, then forgets that agent and closes its MCP
// sessions.
func attach(t *tools, sessions *mcpSessions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := req.Header.Get(agentHeader)
		if id == "" {
			http.Error(w, "no "+agentHeader+" header", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		// Done when dbgp mcp closes the connection or the server stops.
		<-req.Context().Done()
		if err := t.leave(id); err != nil {
			fmt.Fprintf(os.Stderr, "dbgp: agent left: %v\n", err)
		}
		sessions.close(id)
	})
}

// mcpSessionHeader carries the id of the MCP session a request belongs to,
// which the MCP library assigns in its answer to initialize.
const mcpSessionHeader = "Mcp-Session-Id"

// mcpSessions records which MCP sessions each agent uses, to close them when
// it leaves: the MCP library keeps a session until its client ends it, which
// a dbgp mcp that crashed or was killed never does.
type mcpSessions struct {
	server *mcp.Server

	mu      sync.Mutex
	byAgent map[string]map[string]bool // session ids, by agent id
}

// track passes requests on to next, recording the MCP session each is from
// for the agent it is from.
func (m *mcpSessions) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		agent := req.Header.Get(agentHeader)
		m.add(agent, req.Header.Get(mcpSessionHeader))
		next.ServeHTTP(w, req)
		// The answer to initialize, which the session starts with.
		m.add(agent, w.Header().Get(mcpSessionHeader))
	})
}

func (m *mcpSessions) add(agent, session string) {
	if agent == "" || session == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byAgent == nil {
		m.byAgent = map[string]map[string]bool{}
	}
	if m.byAgent[agent] == nil {
		m.byAgent[agent] = map[string]bool{}
	}
	m.byAgent[agent][session] = true
}

// close closes the MCP sessions of the agent with the given id, which has
// left, and forgets them.
func (m *mcpSessions) close(agent string) {
	m.mu.Lock()
	ids := m.byAgent[agent]
	delete(m.byAgent, agent)
	m.mu.Unlock()
	for ss := range m.server.Sessions() {
		if ids[ss.ID()] {
			_ = ss.Close()
		}
	}
}

// agentHeader carries the id of the agent a request is from, which its
// dbgp mcp chooses: see agentID.
const agentHeader = "Dbgp-Agent"

// tokenClient returns an HTTP client that sends token and the agent id to
// the server at rawURL, and to no other. A state file from before tokens
// has none: that server takes requests without one.
func tokenClient(rawURL, token, agent string) (*http.Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &bearerTransport{host: u.Host, token: token, agent: agent}}, nil
}

// bearerTransport adds the token and the agent id to requests for host.
type bearerTransport struct {
	host, token, agent string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == t.host {
		req = req.Clone(req.Context())
		if t.token != "" {
			req.Header.Set("Authorization", "Bearer "+t.token)
		}
		if t.agent != "" {
			req.Header.Set(agentHeader, t.agent)
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

// requireToken passes on only requests that carry token as a bearer token.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dbgp"`)
			http.Error(w, "unauthorized: connect agents with dbgp mcp, which sends the token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// serveHTTP serves MCP with server, whose tools are t, over HTTP on port
// until ctx is done, recording the server in the state file meanwhile.
func serveHTTP(ctx context.Context, server *mcp.Server, t *tools, port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(httpHost, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	st := daemonState{PID: os.Getpid(), URL: "http://" + ln.Addr().String() + "/mcp", Token: rand.Text()}
	if err := writeState(st); err != nil {
		_ = ln.Close()
		return err
	}
	defer removeState(st.PID)

	hs := &http.Server{Handler: httpHandler(server, t, st), ReadHeaderTimeout: 10 * time.Second}
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

// httpHandler serves MCP at /mcp and agents' presence, to requests with st's
// token, and the identity check. t are server's tools.
func httpHandler(server *mcp.Server, t *tools, st daemonState) http.Handler {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{CrossOriginProtection: http.NewCrossOriginProtection()})
	sessions := &mcpSessions{server: server}
	mux := http.NewServeMux()
	mux.Handle("/mcp", requireToken(st.Token, sessions.track(handler)))
	mux.Handle(attachPath, requireToken(st.Token, attach(t, sessions)))
	mux.Handle(identifyPath, identify(st))
	return mux
}
