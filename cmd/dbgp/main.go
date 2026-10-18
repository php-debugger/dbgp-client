// Command dbgp is an interactive debugger for PHP Debugger and Xdebug.
//
// It listens for DBGp connections from PHP and reads commands from stdin;
// type help at the prompt for the list. dbgp mcp runs it as an MCP server
// for AI agents instead.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"

	dbgp "github.com/php-debugger/dbgp-client"
)

// version is "dev" for local builds; release builds set it to the release
// tag with -ldflags "-X main.version=<tag>".
var version = "dev"

// listFlag collects a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ", ") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(runMCP(os.Args[2:]))
	}

	phpPort := flag.Int("php-port", defaultPHPPort, "port to listen on for PHP connections, on every address of this machine")
	idekey := flag.String("idekey", "", "accept only sessions with this IDE key")
	history := flag.String("history", defaultHistoryFile(), "`file` to keep command history in; empty disables it")
	showVersion := flag.Bool("version", false, "print the version and exit")
	var maps, breaks listFlag
	flag.Var(&maps, "map", "path mapping `LOCAL=REMOTE` for PHP in a container or on a server (repeatable)")
	flag.Var(&breaks, "break", "breakpoint at `FILE:LINE` (repeatable)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: dbgp [options]\n       dbgp mcp [options]\n\n"+
			"Interactive debugger for PHP Debugger and Xdebug. Start PHP with debugging\n"+
			"enabled and pointed at -php-port; type help at the prompt for commands.\n"+
			"dbgp mcp runs it as an MCP server for AI agents instead (dbgp mcp -h).\n\nOptions:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("dbgp", version)
		return
	}

	pathMap, err := parsePathMap(maps)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := checkPort("-php-port", *phpPort); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	srv, err := dbgp.Listen(dbgp.Config{Addr: phpAddr(*phpPort), IDEKey: *idekey, PathMap: pathMap})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer srv.Close()

	// Line editing, history and completion on a terminal; plain lines from a
	// pipe or file.
	var lines lineReader
	var out io.Writer = os.Stdout
	var term *terminalReader
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		if term, err = newTerminalReader(*history); err != nil {
			fmt.Fprintf(os.Stderr, "line editing unavailable: %v\n", err)
		} else {
			defer term.rl.Close()
			lines, out = term, term.rl.Stdout()
		}
	}
	r := newREPL(srv, out)
	if term != nil {
		r.onPrompt = term.rl.SetPrompt
	}
	if lines == nil {
		lines = newPlainReader(r, os.Stdin)
	}
	for _, b := range breaks {
		r.execLine("break " + b)
	}
	r.printf("Listening for PHP on port %d. Sessions are announced when PHP connects,\n"+
		"and commands that need one wait for it. Type help for commands.\n", srv.Port())
	r.printListenWarnings()

	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	go r.cancelOnInterrupt(interrupts)
	go r.announceSessions()
	r.run(lines)
}

// defaultPHPPort is where PHP Debugger and Xdebug connect by default.
const defaultPHPPort = 9003

// phpAddr is the address to listen on for PHP: every address of this
// machine, so that PHP in a container, a VM or on another machine can
// connect as well as PHP on this one.
func phpAddr(port int) string {
	return net.JoinHostPort("", strconv.Itoa(port))
}

// checkPort reports an error unless port, the value of the named flag, is
// a TCP port or 0 for any free one.
func checkPort(name string, port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("invalid %s %d: want 1 to 65535, or 0 for any free port", name, port)
	}
	return nil
}

// parsePathMap parses LOCAL=REMOTE flags.
func parsePathMap(specs []string) ([]dbgp.PathMapping, error) {
	var m []dbgp.PathMapping
	for _, spec := range specs {
		pm, err := parseMapping(spec)
		if err != nil {
			return nil, fmt.Errorf("invalid -map %q: %w", spec, err)
		}
		m = append(m, pm)
	}
	return m, nil
}

// parseMapping parses LOCAL=REMOTE, making LOCAL absolute.
func parseMapping(spec string) (dbgp.PathMapping, error) {
	local, remote, ok := strings.Cut(spec, "=")
	local, remote = strings.TrimSpace(local), strings.TrimSpace(remote)
	if !ok || local == "" || remote == "" {
		return dbgp.PathMapping{}, errors.New("want LOCAL=REMOTE")
	}
	local, err := filepath.Abs(local)
	if err != nil {
		return dbgp.PathMapping{}, err
	}
	return dbgp.PathMapping{Local: local, Remote: remote}, nil
}
