# DBGP Client

DBGP Client is a debugger client for PHP. Its `dbgp` command speaks DBGp, the protocol of [PHP Debugger](https://php-debugger.dev) and Xdebug, and can be used in two ways:

- **As a debugging tool for AI agents.** `dbgp mcp` is an [MCP](https://modelcontextprotocol.io) server that lets an agent such as Claude Code debug PHP the way you would: set breakpoints, run the code, step through it, and look at the stack, variables, output and warnings.
- **As a command-line DBGp client.** `dbgp` on its own is an interactive debugger in your terminal, for when you want to debug PHP yourself without an IDE.

Both work with PHP scripts and web requests, on your local machine or in a container or on a server.

## Installation

The install script downloads the latest [release](https://github.com/php-debugger/dbgp-client/releases) for your OS and architecture and installs `dbgp` system-wide.

macOS / Linux (installs to `/usr/local/bin`, using `sudo` if needed):

```sh
curl -fsSL https://github.com/php-debugger/dbgp-client/releases/latest/download/install.sh | sh
```

Windows (PowerShell as administrator; installs to `%ProgramFiles%\dbgp` and adds it to the system PATH):

```powershell
powershell -c "irm https://github.com/php-debugger/dbgp-client/releases/latest/download/install.ps1 | iex"
```

Set `INSTALL_DIR` to install elsewhere (on Windows, it is then added to your own PATH rather than the system one), and `VERSION` (e.g. `0.1.1`) to install a release other than the latest. Run the script again to update. Builds are published for Linux and macOS (amd64 and arm64) and Windows (amd64); elsewhere, install with Go:

```sh
go install github.com/php-debugger/dbgp-client/cmd/dbgp@latest
```

`dbgp -version` shows the installed version.

The install scripts check the download against the release's `SHA256SUMS` before installing it. Each release file also has a build provenance attestation, which shows it was built by this repository's release workflow; to check one you downloaded yourself, run `gh attestation verify FILE --repo php-debugger/dbgp-client`.

To be debugged, PHP needs [PHP Debugger](https://php-debugger.dev) or Xdebug: `php -v` shows which one it has.

## Debugging with AI agents

Add `dbgp mcp` to your agent as an MCP server. For example, for Claude Code:

```sh
claude mcp add dbgp -- dbgp mcp
```

Other agents take the same command, `dbgp mcp`, in their MCP settings. Then ask the agent to debug something, for example:

> Find out why the total on the checkout page is wrong: debug the request to /checkout with a breakpoint in CartService::total().

The agent starts the PHP code itself, as a script or a web request, and debugs it with dbgp's tools: breakpoints, running and stepping, the call stack and variables, the source, what the script printed and the PHP warnings it raised, and evaluating PHP code. dbgp only listens for PHP connections while the agent is debugging, so PHP runs at full speed the rest of the time.

You can optionally add some options after `dbgp mcp`:

- `-no-eval` turns off everything that runs PHP code in the debugged script: evaluating expressions, changing variables and breakpoint conditions, so the agent can only look.
- `-map LOCAL=REMOTE` maps a directory on this machine to where PHP sees it, for PHP in a container or on a server. The agent can also add mappings itself.
- `-php-port` changes the port dbgp listens on for PHP, 9003 by default. dbgp listens on every address of this machine, so PHP in a container or on another machine can connect too.

By default each agent session starts its own dbgp. To keep breakpoints and sessions between agent sessions, and share them between agents, run dbgp in the background:

```sh
dbgp mcp start     # start it, with any of the options above
dbgp mcp status    # is it running?
dbgp mcp stop      # stop it
```

`dbgp mcp start` also takes `-mcp-port`, the port agents reach it on, 9004 by default. It listens on this machine only (127.0.0.1).

While it runs, agents that start `dbgp mcp` use it instead, with the options it was started with. Only you can use it: `dbgp mcp` connects with a token kept in a file that other users on the machine cannot read. After updating dbgp, restart it to use the new version.

`dbgp mcp -h` lists all the options.

## Debugging from the command line

Run `dbgp`, optionally with breakpoints, then run the PHP code, for example in another terminal:

```sh
dbgp -break src/Controller/HomeController.php:25
php app.php
```

Each script or request that connects becomes a session, stopped at its start. At the prompt, `run` runs to the next breakpoint, `next`, `step` and `out` step through the code, and `stack`, `vars` and `print $user["name"]` show where it stopped. On a terminal it has line editing, tab completion and history.

Type `help` at the prompt for all the commands, and see `dbgp -h` for the options, such as `-map` for PHP in a container or on a server.

## Security

Like any debugger, dbgp trusts the PHP that connects to it and the agent that uses it:

- **The PHP port.** DBGp has no authentication: anything that can reach the port, 9003 by default, can connect as PHP and show dbgp made-up code, variables and output, which agents then read. dbgp listens on every address of this machine so that PHP in a container or on another machine can connect, so make sure only your PHP hosts and container networks can reach that port, for example with a firewall. `-idekey` accepts only sessions with that IDE key, which keeps out other PHP, but it is sent in plain text and is not a password. To debug PHP across a network you do not trust, connect through an SSH tunnel or a VPN. `dbgp mcp` only listens while an agent is debugging, and every session shows the address it connected from.
- **The agent.** An agent using dbgp can see everything the debugged script can: its variables, including passwords and keys, and any file PHP can read. `-no-eval` stops the agent from running PHP code of its own, but does not hide anything from it.
