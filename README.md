# dbgp-client
CLI DBGP client

Code imported from `cli/dbgp` in https://github.com/php-debugger/php-debugger/pull/12 (authored by @Haehnchen).

## Usage

A `Server` listens for [PHP Debugger](https://php-debugger.dev) or Xdebug connections. Each PHP request or script run becomes a `Session`, which waits at the start of the script until it is continued.

```go
srv, err := dbgp.Listen(dbgp.Config{
	Addr: "0.0.0.0:9003",
	// For PHP in a container or on a remote server: sessions take and
	// return local paths, and the engine sees its own.
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

## Testing

```sh
go test -race ./...
```

- **Unit tests** run the client against a fake debug engine (`fakeengine_test.go`) that parses commands the way Xdebug and PHP Debugger do and replays packets recorded from a real PHP Debugger session (`testdata/dbgp/`).
- **Integration tests** (`integration_test.go`) debug `testdata/php/basic.php` with a real PHP and a DBGp engine: [PHP Debugger](https://php-debugger.dev) (used in CI) or Xdebug. They are skipped when `php` has neither loaded, or with `-short`; set `DBGP_REQUIRE_ENGINE=1` to make that a failure instead.
- **Known bugs** are documented by tests that call `knownBug(...)` and are skipped by default. Run them with `DBGP_KNOWN_BUGS=1 go test -race ./...`; remove the call when the bug is fixed.
- **CI** (`.github/workflows/tests.yml`) runs on pull requests and pushes to `main`: gofmt and `go vet`, then the full suite against the latest PHP Debugger release for PHP 8.2–8.5.
- **Re-recording fixtures**: `DBGP_CAPTURE=1 go test -run TestCaptureFixtures` (records from whichever engine `php` has loaded; CI uses PHP Debugger). Paths and process ids are normalised so recordings are stable.
