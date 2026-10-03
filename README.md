# dbgp-client
CLI DBGP client

Code imported from `cli/dbgp` in https://github.com/php-debugger/php-debugger/pull/12 (authored by @Haehnchen).

## Testing

```sh
go test -race ./...
```

- **Unit tests** run the client against a fake Xdebug engine (`fakeengine_test.go`) that parses commands the way Xdebug does and replays packets recorded from a real Xdebug session (`testdata/xdebug/`).
- **Integration tests** (`integration_test.go`) debug `testdata/php/basic.php` with a real PHP + Xdebug. They are skipped when `php` with the Xdebug extension is not available, or with `-short`.
- **Known bugs** are documented by tests that call `knownBug(...)` and are skipped by default. Run them with `DBGP_KNOWN_BUGS=1 go test -race ./...`; remove the call when the bug is fixed.
- **Re-recording fixtures**: `DBGP_CAPTURE=1 go test -run TestCaptureXdebugFixtures`. Paths and process ids are normalised so recordings are stable.
