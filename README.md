# dbgp-client
CLI DBGP client

Code imported from `cli/dbgp` in https://github.com/php-debugger/php-debugger/pull/12 (authored by @Haehnchen).

## Usage

A `Server` listens for [PHP Debugger](https://php-debugger.dev) or Xdebug connections. Each PHP request or script run becomes a `Session`, which waits at the start of the script until it is continued. `NewServer` creates a server that is not listening yet: until `StartListening`, its port is closed, so PHP's connection is refused at once and scripts run undebugged at full speed. `StopListening` refuses new connections again without affecting connected sessions; `Listen` is `NewServer` plus `StartListening`. `AddPathMapping` adds a path mapping at any time; connected sessions use it at once.

```go
srv, err := dbgp.Listen(dbgp.Config{
	Addr: "0.0.0.0:9003",
	// For PHP in a container or on a remote server: sessions take and
	// return local paths, and the engine sees its own. Not needed when the
	// server has its own .xdebug map files; Session.EngineMapping reports that.
	PathMap: []dbgp.PathMapping{{Local: "/home/me/project", Remote: "/var/www/html"}},
})
// Server breakpoints apply to every session, including ones that connect later.
srv.AddBreakpoint(dbgp.Breakpoint{File: "/home/me/project/index.php", Line: 12})

sess, err := srv.WaitForSession(ctx)
// Continue waits up to the given time; if the script is still running then,
// the state says so and Wait can be called later.
st, err := sess.Continue(ctx, dbgp.ContinueRun, 30*time.Second)
if st.Status == dbgp.StatusBreak {
	stack, _ := sess.GetStack()
	vars, _ := sess.GetContext(0, 0)
}
out, next, _ := sess.Output(0)      // program output, read incrementally
notes, _ := sess.Notifications(0)   // PHP warnings and other notifications
```

## Command-line debugger

`cmd/dbgp` is an interactive debugger built on the library:

```sh
go install github.com/php-debugger/dbgp-client/cmd/dbgp@latest
dbgp -break src/Controller/HomeController.php:25
```

Then start PHP with debugging enabled (e.g. `php -dxdebug.mode=debug -dxdebug.start_with_request=yes app.php`). Sessions are announced as they connect, and commands that need one wait for it, as `run` and the step commands wait for the script to stop (Ctrl-C cancels a wait). It listens from the start; `unlisten` refuses new PHP connections, so scripts run undebugged, and `listen` accepts them again. `map LOCAL=REMOTE` adds a path mapping while debugging (also applied to connected sessions) and `maps` lists them. Type `help` for commands such as `run`, `next`, `step`, `stack`, `vars`, `print $user["name"]`, `eval`, `output` and `notes`. On a terminal it has line editing, tab completion and history (kept in `~/.dbgp_history`; change it with `-history`, or pass `-history ""` to disable it). Other options: `-addr`, `-idekey`, `-map LOCAL=REMOTE` for PHP in a container or on a server, and `-break FILE:LINE`. Commands can also be piped in on stdin.

## MCP server for AI agents

`dbgp mcp` runs the debugger as an [MCP](https://modelcontextprotocol.io) server on stdin and stdout, so an AI agent can debug PHP. To add it to Claude Code:

```sh
claude mcp add dbgp -- dbgp mcp
```

Unlike the interactive mode, it does not listen for PHP connections until the agent calls the `listen` tool, so PHP runs at full speed until there is something to debug; `-listen` starts listening right away. `-addr`, `-idekey` and `-map LOCAL=REMOTE` work as in the interactive mode.

Tools so far: `status` (listening state, sessions, breakpoints and path mappings), `listen` and `unlisten`; `add_breakpoint`, `remove_breakpoint` and `breakpoints`; `add_path_mapping` and `path_mappings`; `sessions`, `wait_for_session`, `continue` (run or step), `wait_for_stop`, `stop` and `detach`; `stack`, `variables`, `variable` (one variable, a page of its contents at a time) and `variable_value`; `source`, `output` and `warnings` (what the script printed and the PHP warnings it raised, each read incrementally); `eval` and `set_variable`, which run PHP code in the debugged script and can be left out with `-no-eval`.

## Testing

```sh
go test -race ./...
```

- **Unit tests** run the client against a fake debug engine (`fakeengine_test.go`) that parses commands the way Xdebug and PHP Debugger do and replays packets recorded from a real PHP Debugger session (`testdata/dbgp/`).
- **Integration tests** (`integration_test.go`) debug `testdata/php/basic.php` with a real PHP and a DBGp engine: [PHP Debugger](https://php-debugger.dev) (used in CI) or Xdebug. They are skipped when `php` has neither loaded, or with `-short`; set `DBGP_REQUIRE_ENGINE=1` to make that a failure instead.
- **Known bugs** are documented by tests that call `knownBug(...)` and are skipped by default. Run them with `DBGP_KNOWN_BUGS=1 go test -race ./...`; remove the call when the bug is fixed.
- **CI** (`.github/workflows/tests.yml`) runs on pull requests and pushes to `main`: gofmt and `go vet`, then the full suite against the latest PHP Debugger release for PHP 8.2–8.5.
- **Re-recording fixtures**: `DBGP_CAPTURE=1 go test -run TestCaptureFixtures` (records from whichever engine `php` has loaded; CI uses PHP Debugger). Paths and process ids are normalised so recordings are stable.
