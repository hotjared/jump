# Deployment and operations

## OIDC

Register a confidential web application at your OIDC provider. Use authorization-code flow and the exact redirect URI in `OIDC_REDIRECT_URI`. Ensure the issuer URL publishes standard discovery and JWKS metadata. For Microsoft Entra, choose an issuer for the desired tenant; Jump does not hardcode Entra. Enforce MFA and permitted users at the provider. The first user to complete sign-in becomes Jump admin, so configure provider access before first launch.

## Reverse proxy

Nginx Proxy Manager runs on a separate machine from the Jump Docker host. Terminate HTTPS/WSS at NPM and forward across the trusted private LAN by HTTP:

| NPM hostname | Scheme and forward host | Forward port | Settings |
| --- | --- | --- | --- |
| `jump.example.com` | `http://<Jump server LAN IP>` | 8000 | TLS, preserve Host and X-Forwarded-Proto |
| `agent.jump.example.com` | `http://<Jump server LAN IP>` | 8080 | TLS, WebSocket support, long timeouts |

Set `JUMP_BIND_ADDRESS=<Jump server LAN IP>` in `.env` (for example `192.168.69.50`); the safe default `127.0.0.1` is reachable only from the Jump host. No shared Docker network with NPM is used. At the Jump host firewall, allow the NPM host to TCP 8000 and 8080 and deny other LAN clients direct access to those ports. Configure the rule on your own firewall platform; Compose does not manage host firewall policy.

On the broker proxy host, enable WebSocket support, use read/send timeouts of at least 120 seconds (longer than the broker heartbeat window), and turn off response buffering for `/connect` if needed. NPM Advanced directives such as `proxy_read_timeout 120s;`, `proxy_send_timeout 120s;`, and `proxy_buffering off;` can be applied to that proxy host. The agent must see an external HTTPS broker URL and uses WSS for its connection; only localhost or 127.0.0.1 may use HTTP during development.

Block `/api/internal/*` from the public Jump UI/API proxy host; it is for authenticated broker-to-Jump traffic over the Compose network. Never route broker `/internal/*` or port 8081 through NPM. PostgreSQL, guacd, and the broker control listener have no host port mapping. Jump calls `http://broker:8081` internally using a bearer token for revocation. The Compose network connects Jump, broker, PostgreSQL, and guacd; it is scoped to this stack and retains outbound access needed for OIDC. If you do not trust the LAN between NPM and Jump, configure TLS for that upstream separately.

## First run

Download `docker-compose.yml` and `.env.example` into one directory on the Jump host; a repository clone is not required. Copy `.env.example` to `.env`; replace all placeholders. Use a URL-safe random hex database password. Generate the master key with `openssl rand -base64 32`. Set `COOKIE_SECURE=true` behind HTTPS and `JUMP_BIND_ADDRESS` to the Jump host's LAN IP. Once the GHCR packages are accessible, run:

```sh
docker compose pull
docker compose up -d
docker compose logs jump broker
```

The Jump container runs `alembic upgrade head` before serving; subsequent starts repeat this safely. PostgreSQL data remains in the `postgres_data` named volume across image updates, including devices, audit events, credentials, and enrollment history. Open the UI and complete OIDC sign-in.

## GHCR packages

Pushes to `main` publish `ghcr.io/hotjared/jump` and `ghcr.io/hotjared/jump-broker` with `latest` and the full commit SHA as tags. A `v0.1.0` tag also publishes `v0.1.0`, `0.1.0`, and `0.1`. The workflow uses `GITHUB_TOKEN` with `contents: read` and `packages: write`; no publishing PAT is needed. Pull requests and feature branches do not publish images.

After the first successful publication, the repository owner should open each package under their GitHub profile's **Packages** section, open **Package settings**, and change its visibility to **Public**. The images link to this repository; if they inherit permissions from the private repository, remove inherited permissions in package settings before changing visibility. This is a manual action for both images. Public GHCR images can be pulled anonymously. If either package stays private, authenticate the production Docker host to GHCR with a personal access token (classic) scoped to `read:packages`, from an account with access to that package. Supply it through `docker login ghcr.io -u hotjared --password-stdin` or a credential helper; do not place it in `.env` or Compose.

## Native agent releases

The canonical agent binaries are GitHub Release assets, not files in the repository or the Jump container. Supported targets are Linux amd64 (`jump-agent-linux-amd64`) and Windows amd64 (`jump-agent-windows-amd64.exe`). The same release includes `SHA256SUMS` with hashes of both exact uploaded binaries. The admin enrollment UI links directly to these assets and shows commands using their published filenames. Enrollment still requires a separate one-time token; download URLs never include it.

A push of a `v*` tag, such as `v0.1.0`, starts the dedicated native agent workflow. It checks out the tag, uses the Go version in `agent/go.mod`, cross-compiles both platforms with the tag injected into agent metadata, validates the checksums, and creates or updates the matching GitHub Release using the built-in `GITHUB_TOKEN`. It uploads all three assets. The existing GHCR workflow independently publishes the corresponding `ghcr.io/hotjared/jump` and `ghcr.io/hotjared/jump-broker` image tags. Maintainers should tag the tested `main` commit and push it, for example `git tag v0.1.0 && git push origin v0.1.0`; confirm both workflows succeed before deploying that version. Feature branches and PRs do not publish releases.

The tagged Jump server image carries its own `v*` version and offers the matching agent release by default. `latest` and commit SHA images identify as development builds; set `JUMP_AGENT_VERSION=v0.1.0` (using a real published tag) in `.env` to select an agent release, then run `docker compose up -d` to apply the setting. The override also works for a tagged server when you deliberately need a different agent release. An invalid override is rejected at startup; with no valid version, the UI shows no download link. The server constructs official `hotjared/jump` asset URLs locally and does not call GitHub's API when rendering enrollment.

Download `SHA256SUMS` and verify the selected Linux file in the same directory:

```sh
grep ' jump-agent-linux-amd64$' SHA256SUMS | sha256sum -c -
chmod +x jump-agent-linux-amd64
```

On Windows, run `Get-FileHash .\jump-agent-windows-amd64.exe -Algorithm SHA256` in PowerShell and compare the output with the Windows line in `SHA256SUMS`. Windows builds are currently unsigned and may show a Microsoft SmartScreen warning. Inspect the release and checksum before choosing to run them; no code-signing certificate, workaround, or suppression is part of this release process. The repository must permit intended users to access the GitHub Release assets; if it remains private, GitHub will require repository access for downloads.

## Update and rollback

Set `JUMP_VERSION=latest` to follow `main`, or pin both images to the same release tag such as `JUMP_VERSION=v0.1.0` or a full commit SHA. Then run:

```sh
docker compose pull
docker compose up -d
```

To roll back, restore the previous `JUMP_VERSION` and repeat those commands. Back up PostgreSQL and `JUMP_MASTER_KEY` before upgrades: a future Alembic migration may make an older application image incompatible with the upgraded database schema, so an image-only rollback may be unsafe.

Download the agent from the enrollment UI and use the displayed one-time enrollment command, then install the native service.

Linux amd64:

```sh
chmod +x jump-agent-linux-amd64
sudo ./jump-agent-linux-amd64 enroll --server https://agent.example.com --token <TOKEN>
sudo ./jump-agent-linux-amd64 service install
```

The install command copies the binary to `/usr/local/bin/jump-agent`, writes `/etc/systemd/system/jump-agent.service`, reloads systemd, and enables/starts the service. The unit runs `jump-agent run`, starts at boot, and restarts on failure. The identity remains at `/var/lib/jump-agent/identity.json` with the existing restrictive directory/file permissions. `sudo jump-agent service stop|start|uninstall` manages the service; uninstall disables and removes the unit but does not remove the identity.

Windows amd64, from elevated PowerShell:

```powershell
.\jump-agent-windows-amd64.exe enroll --server https://agent.example.com --token <TOKEN>
.\jump-agent-windows-amd64.exe service install
```

The install command copies the agent under Program Files and registers the native `JumpAgent` Windows service with display name `Jump Agent`. It runs as LocalSystem, starts automatically, and is configured to restart after failures. Identity remains at `%ProgramData%\Jump\identity.json`; the existing ACL hardening to SYSTEM and Administrators is preserved. `service uninstall` removes the service registration but intentionally leaves the identity in place.

For both platforms, `jump-agent run` remains available as a foreground troubleshooting/development command. Closing that foreground terminal stops that diagnostic process; the normal installation path is the native service.

To remove an agent’s access, open its device details as an admin and choose **Revoke agent identity**. Jump retains the device and audit history, marks it offline, rejects the old private key permanently, and asks the broker to close any live connection. If the API reports that the broker disconnect could not be confirmed, the key is still revoked in PostgreSQL; retry the same action when the broker is available. Revocation does not delete the device.

After revocation, an offline device can optionally be permanently deleted from its device details. Jump requires a separate delete action and exact-name confirmation; deletion never silently performs revocation. The device record, agent identity, device credentials, and device/tag relationship rows are removed, while shared groups, tags, users, and the durable deletion audit event remain. Deletion is irreversible. Because the device and identity no longer exist, the previously revoked private key remains unusable and broker authentication returns unknown/rejected. A replacement agent must be enrolled as a new device with a new one-time token and local keypair.

## Backup and restore

Back up PostgreSQL with a custom-format dump:

```sh
docker compose exec -T postgres pg_dump -U jump -Fc jump > jump.dump
```

Restore into an empty database after stopping Jump and broker:

```sh
cat jump.dump | docker compose exec -T postgres pg_restore -U jump -d jump --clean --if-exists
docker compose up -d
```

Back up `JUMP_MASTER_KEY` separately in a secure secret store. **A database dump without the master key cannot decrypt credentials. Losing that key permanently makes existing encrypted credentials unrecoverable.** Also protect the OIDC client secret, session secret, and broker token. After a restore, the broker reconciles stale online state; agents reconnect automatically.

## Development

Backend: `cd app/backend && uv pip install -e '.[dev]' && alembic upgrade head && pytest && ruff check . && ruff format --check .`. Frontend: `cd app/frontend && npm ci && npm test && npm run typecheck && npm run build`. Go: in `agent` and `broker`, run `go test ./... && go vet ./... && go build ./...`. To build a local agent from source, run `cd agent && go build -o jump-agent .`; it reports `dev` in metadata. For local image builds use `docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build`. The CI integration script uses the same override. The default production Compose file always pulls prebuilt images.
## SSH terminal prerequisites

Install and run an SSH server on each Linux target. The Jump agent connects to
`127.0.0.1:22`; you do not need to expose port 22 to the Internet. In the
device's Terminal tab as a Jump admin, save a Jump-managed password or SSH private key
credential. The selected credential is encrypted at rest and decrypted only
while opening that session.

Jump pins the server's SHA256 SSH host key fingerprint on the first successful
connection. Check the displayed fingerprint against the target if you need
stronger assurance than Trust On First Use. A mismatch blocks access. An admin
can reset the trusted key in Terminal after verifying an intentional rotation.

Sessions have a 30-minute idle timeout (no input or output), do not record
terminal content, and cannot be resumed. Closing the browser terminal or
disconnecting the agent ends the SSH connection. Keep the broker's internal
port 8081 private to the Compose network and protect `BROKER_INTERNAL_TOKEN`.
