# Jump

Browser RDP setup and limitations: [Browser RDP (Phase 2)](docs/browser-rdp.md).

Jump is a self-hosted remote access foundation for a single environment. It provides OIDC or local sign-in, device enrollment, persistent outbound agent presence, device organization, audit events, RDP, files and browser SSH terminals on Linux.

## Components

- **jump** — FastAPI, React, OIDC, PostgreSQL models and migrations.
- **broker** — Go WebSocket service for enrolled agents and presence.
- **agent** — Go executable for Windows amd64 and Linux amd64.
- **guacd** — official Apache Guacamole protocol daemon, reserved for Phase 2. The Guacamole web app is not included.
- **postgres** — PostgreSQL 18 in a named volume.

See [architecture](docs/architecture.md), [deployment](docs/deployment.md), and [security](SECURITY.md).

## Interface preview

Screenshots use illustrative device data:

![Devices page](docs/screenshots/devices.png)

[Device details](docs/screenshots/device-details.png) · [Dashboard](docs/screenshots/dashboard.png)

## Quick start

1. Download [`docker-compose.yml`](docker-compose.yml) and [`.env.example`](.env.example) into a directory on the Jump host; no source checkout is needed. Copy `.env.example` to `.env`. Generate independent secrets: `openssl rand -hex 32` for the database password, session secret and broker internal token, and `openssl rand -base64 32` for `JUMP_MASTER_KEY`. Back up the master key separately.
2. Choose `AUTH_MODE=oidc` (default), `AUTH_MODE=local`, or `AUTH_MODE=hybrid` in `.env`. For OIDC or hybrid, configure an OIDC client with redirect URI `https://jump.example.com/auth/callback` and set issuer, client ID and secret. Local mode needs no OIDC variables. Set your real HTTPS URLs and allowed hostname.
3. Set `JUMP_BIND_ADDRESS` to the Jump host's private LAN IP when Nginx Proxy Manager runs on another host. Allow only that NPM host to reach TCP 8000 and 8080 on the Jump host. Configure HTTPS, WebSocket support on the agent host, and the [proxy settings](docs/deployment.md).
4. Run `docker compose pull && docker compose up -d`. The application applies Alembic migrations before starting. The production Compose file uses `ghcr.io/hotjared/jump` and `ghcr.io/hotjared/jump-broker`; it does not build locally. The repository owner must make both GHCR packages public after their first publication, or the server must authenticate with a read-only package token.
5. Sign in. With OIDC, **the first successfully authenticated user becomes admin**; configure the provider to admit only intended users before exposing Jump. For an empty local or hybrid installation, open the login page, read the one-time setup token with `docker compose logs jump`, and complete the administrator setup form. There is no default local account. Existing OIDC users suppress local bootstrap in hybrid mode.
6. In **Devices → Enroll device**, choose an OS, download the native agent, generate a token, and run the displayed enrollment and native service-install commands on the endpoint. The token expires after 15 minutes and is shown once. The installed service keeps the device online after the terminal closes and across reboots.

The agent defaults to `/var/lib/jump-agent/identity.json` on Linux and `%ProgramData%\Jump\identity.json` on Windows. After enrollment, run `service install` from an elevated shell. Linux installs `/usr/local/bin/jump-agent` plus `jump-agent.service`; Windows installs the `JumpAgent` service running as LocalSystem. `service uninstall` removes the service configuration but intentionally preserves the enrolled identity. The foreground `run` command remains available for diagnostics and development. Use `JUMP_AGENT_STATE` to set another path for foreground/development use. The broker only accepts inbound requests through the reverse proxy; endpoints initiate all connections.

For upgrades, rollback, GHCR visibility, and local image builds for development, see [deployment and operations](docs/deployment.md).

OIDC is recommended when you already have an identity provider. Local auth runs without an external IdP. `AUTH_MODE=oidc` does not expose the local password login endpoint. Local passwords do not have MFA in this release; user management and local MFA are planned separately.

Admins can update an older `agent_update_v1` service from its device details when a newer stable native agent release is published. Agents installed before this capability need [one manual upgrade](docs/deployment.md#administrator-initiated-agent-updates) first. Jump blocks updates during active SSH sessions.

## Native agent downloads

Jump offers Linux amd64 and Windows amd64 binaries from the latest usable stable [GitHub Release](https://github.com/hotjared/jump/releases). The enrollment UI links to the selected binary and `SHA256SUMS`; no Go toolchain or source checkout is needed on endpoints. Jump checks for new releases about every five minutes, independently of the server image tag. If GitHub is temporarily unavailable, it keeps the last validated release. Until a release has been validated, the UI offers no download or update.

Download `SHA256SUMS` alongside the binary. On Linux, verify the selected entry with:

```sh
grep ' jump-agent-linux-amd64$' SHA256SUMS | sha256sum -c -
chmod +x jump-agent-linux-amd64
```

On Windows, run `Get-FileHash .\jump-agent-windows-amd64.exe -Algorithm SHA256` in PowerShell and compare its hash with the Windows line in `SHA256SUMS`. Windows binaries are currently unsigned and may trigger Microsoft SmartScreen. Check the release source and hash before deciding whether to run one; Jump does not bypass Windows warnings.

### Agent lifecycle

Normal endpoint setup is:

```text
download
enroll
service install
automatic startup
revoke if compromised/decommissioned
optional delete from Jump
```

Linux uses native systemd with automatic startup and restart-on-failure. Windows uses the native Service Control Manager with the stable service name `JumpAgent`, display name `Jump Agent`, automatic startup, LocalSystem, and restart-on-failure recovery actions. No third-party service wrapper is required.

Revocation permanently invalidates the enrolled agent identity and disconnects any active broker connection. A revoked, offline device can then be permanently deleted from Jump. Deletion is irreversible, does not revoke implicitly, and does not make the deleted agent key usable again; the broker treats the removed identity as unknown.

## Build agent from source (developers)

```sh
cd agent
go build -o jump-agent .
GOOS=windows GOARCH=amd64 go build -o jump-agent.exe .
```

Source builds report `dev`; release builds report the tag in the device metadata.

## Status

This is an early security-sensitive foundation. Review [known limitations](docs/architecture.md#known-limitations) before exposing it to the Internet.
## Browser SSH sessions

As a Jump admin, open **Terminal** on an enrolled Linux device, save a password or unencrypted
SSH private key credential, then choose it and select **Connect**. SSH must be
installed and listening on `127.0.0.1:22` on that device. The agent opens the
local SSH connection; no inbound Internet SSH port is needed.

The path is browser → Jump session gateway → private broker → existing
authenticated agent connection → localhost SSH. Multiple terminal sessions
share the same agent connection. Saved secrets are encrypted at rest with
`JUMP_MASTER_KEY` and decrypted only to initialize the selected session; they
are never sent to the browser or stored by the agent. Jump trusts the first
successfully authenticated SSH host key (TOFU) and shows its SHA256 fingerprint
in Terminal. A changed key is rejected until an admin verifies it and resets
the stored key. Sessions close after 30 minutes without input or output, or
when the browser or agent disconnects. Terminal contents are not recorded;
sessions cannot be resumed.

### Agent-based file transfer (Phase 1)

Devices running an agent that advertises `file_transfer_v1` expose a Files tab in Device Details. The Files action in an active SSH or RDP toolbar opens the same device browser. A remote session is never required: the backend sends admin-authorized operations through the private broker and the device's existing authenticated, persistent agent WebSocket. Older agents continue to connect and show an update-required message in Files.

The agent uses its **service account privileges** for all file access. On Linux, the browser begins at `/`; normal POSIX filenames are supported except NUL, `/`, and traversal components (`.` and `..`). On Windows, the root view lists local fixed drives; stricter Windows component rules apply, and UNC shares and device namespaces are unsupported. The agent validates each path independently, uses `os.Root` for traversal-resistant access within a local root, omits symlinks and special entries from directory listings, and accepts only regular files for downloads. File operations never invoke a shell. An administrator should grant the agent service account only the filesystem access appropriate for the managed device.

One regular file can be uploaded or downloaded at a time from the UI, with up to four simultaneous device-level transfers across users. Directory listings are paged at 32 entries; their temporary authorization claims are removed when browsing finishes and do not accumulate in transfer history. Uploads and downloads stream 32 KiB chunks with bounded WebSocket queues; downloads acknowledge each chunk before the agent reads more. File contents are not stored in Jump's database or written to the broker's logs. Uploads create a temporary file in the destination directory, verify its byte count, flush it, then publish it. Existing files require an explicit overwrite confirmation; otherwise an atomic hard link prevents a concurrent overwrite. Cancelling or disconnecting removes an incomplete temporary file. Both directions compute SHA-256 as bytes flow and verify the result at completion. Transfer progress, final state and sanitized failure codes are available to the initiating admin; start, completion, failure and cancellation events are audited without file contents or chunks.

Jump file transfer is agent-based and does not use RDP drive redirection, Guacamole file transfer, SSH/SFTP, SMB, or inbound filesystem services. Recursive folders, network shares, file edits, renames, deletion and remote execution are outside this phase.
