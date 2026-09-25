# Jump

Jump is a self-hosted remote access foundation for a single environment. This first release provides OIDC sign-in, device enrollment, persistent outbound agent presence, device organization, and audit events. **Remote RDP, SSH, files, and actions are not implemented yet.**

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
2. Configure an OIDC client with the redirect URI `https://jump.example.com/auth/callback`. Set issuer, client ID and secret in `.env`. Set your real HTTPS URLs and allowed hostname.
3. Set `JUMP_BIND_ADDRESS` to the Jump host's private LAN IP when Nginx Proxy Manager runs on another host. Allow only that NPM host to reach TCP 8000 and 8080 on the Jump host. Configure HTTPS, WebSocket support on the agent host, and the [proxy settings](docs/deployment.md).
4. Run `docker compose pull && docker compose up -d`. The application applies Alembic migrations before starting. The production Compose file uses `ghcr.io/hotjared/jump` and `ghcr.io/hotjared/jump-broker`; it does not build locally. The repository owner must make both GHCR packages public after their first publication, or the server must authenticate with a read-only package token.
5. Sign in. **The first successfully authenticated OIDC user becomes admin.** Subsequent users are created with the `user` role. Configure the identity provider to admit only intended users before exposing Jump.
6. In **Devices → Enroll device**, choose an OS, generate a token, run the displayed command on the endpoint, then run `jump-agent run`. The token expires after 15 minutes and is shown once.

The agent defaults to `/var/lib/jump-agent/identity.json` on Linux and `%ProgramData%\Jump\identity.json` on Windows. Run it under a dedicated service account or as an administrator. Use `JUMP_AGENT_STATE` to set another path. The broker only accepts inbound requests through the reverse proxy; endpoints initiate all connections.

For upgrades, rollback, GHCR visibility, and local image builds for development, see [deployment and operations](docs/deployment.md).

## Build agent

```sh
cd agent
go build -o jump-agent .
GOOS=windows GOARCH=amd64 go build -o jump-agent.exe .
```

For service deployment, use systemd on Linux or Windows Service Control Manager with a service wrapper. Restrict the identity file to the service account and administrators.

## Status

This is an early security-sensitive foundation. Review [known limitations](docs/architecture.md#known-limitations) before exposing it to the Internet.
