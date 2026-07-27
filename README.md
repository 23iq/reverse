# Reverse

Reverse exposes a local HTTP service through a VPS you control. It fills the
same basic role as `ngrok http`, but the gateway, credentials, TLS certificate,
and traffic stay on your own machines.

```text
reverse -p 3000

https://tunnel.example.com  ->  http://127.0.0.1:3000
```

It supports ordinary HTTP requests, request and response streaming, and
WebSocket upgrades. Reverse does not provide a hosted control plane, user
accounts, or per-request and per-bandwidth quotas. Your VPS bandwidth, CPU,
memory, file descriptor limits, provider policies, and the client connection
still set practical limits.

Reverse is an HTTP tunnel. It is not a general-purpose TCP or UDP forwarder.

## How it works

```text
Browser or webhook sender
          |
          | HTTPS :443
          v
Caddy on the VPS
  - uses the certificate issued by Certbot
  - terminates TLS
          |
          | HTTP on 127.0.0.1:8787
          v
Reverse server container
          |
          | authenticated WSS + yamux
          v
Reverse client on your computer
          |
          | HTTP
          v
127.0.0.1:3000
```

The client makes an outbound secure WebSocket connection to
`/_reverse/tunnel`. The server checks the configured domain and password, then
wraps the connection in yamux. Each public HTTP request gets its own yamux
stream to the client. A separate control stream carries request events to the
dashboard.

Before accepting a tunnel, the server also reserves the requested TCP port on
the VPS loopback interface as a conflict guard. With `reverse -p 3000`, port
`3000` must have a service listening on the client while remaining unused on
`127.0.0.1` on the VPS. This listener is not publicly exposed; public traffic
goes through Caddy and HTTPS on the configured domain.

## Requirements

### VPS

- A Linux server using systemd.
- An `apt-get`, `dnf`, or `pacman` based distribution.
- A public IPv4 or IPv6 address.
- A fully qualified domain or subdomain with a public A or AAAA record pointing
  to that VPS.
- TCP ports 80 and 443 available and allowed through the firewall.
- `127.0.0.1:8787` available for the loopback gateway.
- Outbound network access for package installation, Docker image builds, and
  Let's Encrypt.
- Root access for `reverse --setup`.

The setup command installs Docker, Caddy, Certbot, and the ACL utilities needed
for certificate access. Git, Make, and Go are still needed to clone and build
the `reverse` binary itself. The module currently targets Go 1.24.

DNS should be in place before setup. If an AAAA record exists, it must point to
this VPS as well. Setup checks public DNS and compares it with addresses
detected on the server.

Reverse will not stop an unknown process merely to take its port. It also
refuses to overwrite an unmanaged `/etc/caddy/Caddyfile`, stop an active
unmanaged Caddy service, or replace a container named `reverse-server` unless
it is marked as managed by Reverse.

### Client

- The `reverse` binary.
- Outbound HTTPS and WSS access to the configured domain on port 443.
- A local HTTP service listening on the selected address and port.
- The same password entered during VPS setup.

## Build and install

Use the same source tree to build the VPS and client binaries:

```sh
git clone https://github.com/23iq/reverse.git
cd reverse
make build
sudo install -Dm0755 bin/reverse /usr/local/bin/reverse
reverse --version
```

`make build` writes the binary to `bin/reverse`. If you do not want a
system-wide installation, place that file somewhere already listed in your
`PATH`.

## Set up the VPS

Run setup from the repository root. The Docker build needs the `Dockerfile` and
the rest of the source tree in the current directory.

```sh
cd /path/to/reverse
sudo reverse --setup
```

The terminal interface asks for:

1. The domain or subdomain already pointing to the VPS, such as
   `tunnel.example.com`.
2. A non-empty authentication password.

The password is not trimmed, so spaces and non-ASCII characters are valid.
Newlines and NUL bytes are rejected. Store it somewhere safe; the client needs
the exact same value.

Set `REVERSE_EMAIL` if you want Let's Encrypt registration and expiry notices
associated with an email address:

```sh
cd /path/to/reverse
sudo env REVERSE_EMAIL=admin@example.com reverse --setup
```

For an unattended run, pass both credentials through the environment:

```sh
cd /path/to/reverse
sudo env \
  REVERSE_DOMAIN=tunnel.example.com \
  REVERSE_PASSWORD='use-a-long-random-password' \
  REVERSE_EMAIL=admin@example.com \
  reverse --setup
```

Environment variables can be visible to privileged process inspection and
shell history, depending on how they are supplied. Prefer the interactive form
for routine setup.

To inspect the planned commands without DNS lookups, port binds, file writes,
or command execution, add `--dry-run`:

```sh
sudo env \
  REVERSE_DOMAIN=tunnel.example.com \
  REVERSE_PASSWORD='use-a-long-random-password' \
  reverse --setup --dry-run
```

### What setup changes

Setup performs the following work:

- Verifies the domain and required ports.
- Installs Docker, Caddy, Certbot, and ACL tools with the detected package
  manager.
- Enables Docker through systemd.
- Obtains a Let's Encrypt certificate with Certbot's standalone HTTP
  challenge, or reuses a matching certificate that is valid for more than
  seven days.
- Grants the `caddy` user read-only ACL access to the selected certificate
  lineage and installs renewal hooks.
- Writes `/etc/reverse/server.json` with mode `0600`. It contains the domain,
  listener settings, and a one-way password hash.
- Builds the local Docker image `reverse-server:local`.
- Starts the `reverse-server` container with host networking and an
  `unless-stopped` restart policy.
- Writes `/etc/caddy/Caddyfile` with mode `0644`, validates it, and enables
  Caddy.

The generated Caddyfile serves the configured domain with the files at:

```text
/etc/letsencrypt/live/<domain>/fullchain.pem
/etc/letsencrypt/live/<domain>/privkey.pem
```

It proxies HTTPS traffic to the Reverse gateway on `127.0.0.1:8787`.

The container has a read-only root filesystem, enables
`no-new-privileges`, mounts `/etc/reverse/server.json` read-only with Docker's
private `Z` label for SELinux hosts, limits the process count, and rotates its
local JSON logs. Setup owns that file and allows Docker to relabel it for this
container. A small, fail-closed init process reads the root-only configuration
and immediately drops to UID and GID 65532. The Reverse server itself keeps only
`NET_BIND_SERVICE`, which is needed when it checks or reserves a requested
loopback port below 1024.

Host networking lets Caddy on the VPS reach the loopback gateway at port 8787
and lets Reverse reserve the client-requested loopback port without exposing
either listener on a public interface.

Setup marks its Caddyfile and container so it can recognize them on a later
run. It refuses to overwrite an unmanaged Caddyfile, stop an active unmanaged
Caddy service, or replace an unmanaged container with the same name. It
temporarily preserves the previous managed container under a backup name and
attempts to restore files, services, and the old container if the update fails.
Rollback is best effort, so read the final setup error before assuming that
every step was restored.

The generated Caddyfile is owned by setup and may be overwritten on the next
run. Back up any manual changes before running setup again.

### Certificate renewal

Setup obtains the initial certificate and reuses an existing certificate while
it has more than seven days left. It writes executable hooks at:

```text
/etc/letsencrypt/renewal-hooks/pre/reverse-caddy
/etc/letsencrypt/renewal-hooks/deploy/reverse-caddy
/etc/letsencrypt/renewal-hooks/post/reverse-caddy
```

The pre hook stops Caddy only when it is running with the setup-managed
Caddyfile. This lets Certbot's standalone HTTP challenge claim port 80. The
deploy hook reapplies read-only certificate ACLs for the configured domain,
and the post hook starts Caddy only when the pre hook stopped it. A successful
renewal therefore causes a brief HTTPS interruption while Caddy is stopped.

Setup tries to enable `certbot.timer`, then `certbot-renew.timer`. If neither
unit exists, setup completes with a warning and you must schedule
`certbot renew` as root through cron or another timer. The installed hooks are
used either way.

Check the active timer and certificate:

```sh
sudo systemctl list-timers 'certbot*'
sudo certbot certificates
```

Test the complete renewal path during a maintenance window:

```sh
sudo certbot renew --dry-run
sudo systemctl status caddy
```

The dry run invokes the hooks and briefly stops setup-managed Caddy. After a
real renewal, verify the certificate served by the domain.

## Configure a client

Run either spelling:

```sh
reverse --config
reverse -cf
```

Enter the VPS domain and the exact setup password. A bare hostname and an
`https://` URL are accepted; client configuration always normalizes the result
to HTTPS.

On Linux, the file is normally written to:

```text
~/.config/reverse/config.json
```

The exact directory follows the operating system's user configuration
convention and can be overridden with `REVERSE_CONFIG`. Reverse creates the
directory with user-only permissions and the file with mode `0600` on Unix.
The password is stored in this client file because it is needed for future
connections. Do not copy the file into a repository or share it.

Configuration can also be supplied non-interactively:

```sh
REVERSE_DOMAIN=tunnel.example.com \
REVERSE_PASSWORD='use-a-long-random-password' \
reverse --config
```

After saving, Reverse probes `https://<domain>/_reverse/health`. A failed probe
is reported as a warning; the configuration is still saved so DNS or firewall
work can be completed afterward.

## Start a tunnel

Start the local application first, then run:

```sh
reverse -p 3000
```

Open the configured URL:

```text
https://tunnel.example.com
```

The default local target is `127.0.0.1`. Use `--host` when the application is
listening on another local interface:

```sh
reverse --host 192.168.1.20 -p 8080
```

Reverse checks that the local target accepts a TCP connection before opening
the dashboard. Once connected, the dashboard shows:

- Online and reconnecting state.
- Public URL and local target.
- Uptime.
- Incoming and outgoing byte totals.
- Request and error counts.
- Access log entries with client address, method, path, status, duration, and
  transferred bytes.

Press `q`, `Esc`, or `Ctrl+C` to close the dashboard and stop forwarding.
Arrow keys, `k`, Page Up, and End control the access log viewport.

Run `reverse`, `reverse -h`, or `reverse --help` to see the command summary.
`reverse --version` prints the installed version.

## Port and session behavior

The current command uses one port number for both ends:

```text
reverse -p 3000
        |    |
        |    +-- reserve TCP 3000 on VPS loopback
        +------- connect to the configured local host on TCP 3000
```

There is no separate public-port mapping option. Ports 80 and 443 are already
used by Caddy, so run the local application on another port.

If the requested VPS port is occupied, the server rejects the tunnel before
accepting it. Reverse never closes or modifies the existing listener. Find the
owner with:

```sh
sudo ss -ltnp 'sport = :3000'
```

The reserved port is bound to `127.0.0.1`, not a public VPS interface. Do not
open application ports such as 3000 in the firewall; Reverse only needs public
inbound access on ports 80 and 443. Port 8787 also remains loopback-only.

A server accepts one active Reverse client at a time. A second client stays in
the reconnect loop until the active client disconnects. One client may carry
many concurrent HTTP requests and WebSockets through yamux.

The paths `/_reverse/tunnel` and `/_reverse/health` belong to the gateway and
are not forwarded to the local application.

The client reconnects after temporary network and server failures. A wrong
password and an occupied VPS port are configuration errors and are not retried
indefinitely.

## Operations

Useful checks on the VPS:

```sh
curl -fsS https://tunnel.example.com/_reverse/health
sudo docker ps --filter name=reverse-server
sudo docker logs -f reverse-server
sudo systemctl status docker caddy
sudo journalctl -u caddy -f
sudo ss -ltnp
```

The server writes structured request and tunnel events to the container log.
The health endpoint reports whether the process is healthy and whether a
tunnel client is online.

To update an installation:

```sh
cd /path/to/reverse
git pull
make build
sudo install -Dm0755 bin/reverse /usr/local/bin/reverse
sudo reverse --setup
```

Setup asks for the domain and password again, rebuilds the server image, and
checks the replacement's loopback health endpoint before Caddy sends traffic
to it. It uses managed-component checks and a rollback path during replacement.
Run it from the updated repository root.

To change the password, rerun VPS setup and then run `reverse --config` on the
client. The old client password stops working as soon as the replacement
server starts.

## Troubleshooting

### `reverse` says it is not configured

Run `reverse --config` or `reverse -cf` as the user who will start tunnels.
Running it with `sudo` creates a root-owned configuration in root's config
directory, which is usually not what you want.

### Nothing is listening locally

Confirm the application is running and bound to the address passed to Reverse:

```sh
curl -v http://127.0.0.1:3000/
ss -ltn 'sport = :3000'
```

Use `--host` if the service is not bound to `127.0.0.1`.

### Authentication fails

Run `reverse --config` again and enter the exact domain and password used
during VPS setup. Password spaces and capitalization are significant.

### DNS validation fails during setup

Inspect every public record:

```sh
dig +short A tunnel.example.com
dig +short AAAA tunnel.example.com
```

Remove stale records or wait for DNS propagation. Port forwarding on a home
router does not make a private address a valid VPS DNS target.

### Certbot cannot issue or renew a certificate

Check that the domain resolves to the VPS, inbound TCP 80 is open, and no
unmanaged process owns port 80. Let's Encrypt must be able to reach the
standalone challenge from the public Internet.

### The VPS port is occupied

Reverse reports the selected port and leaves its owner untouched. Stop or
reconfigure that service, or move the local application and Reverse to another
port. Because local and VPS port numbers currently match, choosing a different
VPS port also means using that port locally.

### The tunnel remains offline or returns 502

Check the client dashboard first. Then inspect:

```sh
curl -fsS https://tunnel.example.com/_reverse/health
sudo docker logs --tail 200 reverse-server
sudo journalctl -u caddy --since '10 minutes ago'
```

A 502 usually means the client disconnected, yamux could not open a stream, or
the client could no longer reach its local HTTP service.

### Another tunnel is active

Stop the previous client with `q` or `Ctrl+C`. If the old process disappeared
without a clean shutdown, the server clears it when the WebSocket connection
times out or closes, and the waiting client reconnects.

## Security notes

- Use a long, unique tunnel password. The server stores a salted password hash,
  not the plaintext password.
- The client must retain the plaintext password in its mode-`0600`
  configuration file. Protect the user account and backups containing that
  file.
- The credential and tunnel travel over WSS. Do not bypass certificate
  validation or expose the loopback gateway directly.
- The tunnel password authenticates the Reverse client. It does not add login
  protection to the public website. Anyone who can reach the public URL can
  reach the local application unless that application provides its own access
  control.
- Setup runs as root because it installs packages, writes system
  configuration, manages services, and starts Docker containers. Review
  `reverse --setup --dry-run` before using it on a VPS with an existing web
  stack.
- Keep inbound firewall access limited to the services you intend to expose.
  A standard Reverse installation needs public TCP 80 and 443, not the local
  application port or 8787.
- Keep the VPS, Docker, Caddy, Certbot, and the Reverse binary updated.

## Development

Common project commands:

```sh
make build
make test
make test-race
make vet
```

The integration tests create loopback TCP and WebSocket listeners. Run them in
an environment that permits local network binds.

The Docker build context is an allowlist containing only the module files and
Go source trees. If a new source directory becomes part of the server build,
add it to both `Dockerfile` and `.dockerignore`; do not replace the explicit
copies with `COPY . .`.

The tunnel implementation lives in `internal/tunnel`; setup and generated
deployment files are in `internal/setup`; terminal models are in `internal/ui`.

Reverse is distributed under the MIT License. See `LICENSE`.
