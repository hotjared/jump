# Deployment and operations

## OIDC

Register a confidential web application at your OIDC provider. Use authorization-code flow and the exact redirect URI in `OIDC_REDIRECT_URI`. Ensure the issuer URL publishes standard discovery and JWKS metadata. For Microsoft Entra, choose an issuer for the desired tenant; Jump does not hardcode Entra. Enforce MFA and permitted users at the provider. The first user to complete sign-in becomes Jump admin, so configure provider access before first launch.

## Reverse proxy

Run Nginx Proxy Manager with HTTPS for two distinct hosts:

| Host | Upstream | Settings |
| --- | --- | --- |
| `jump.example.com` | Jump port 8000 | TLS, preserve Host and X-Forwarded-Proto |
| `agent.jump.example.com` | Broker port 8080 | TLS, WebSocket support, read/send timeouts above 90 seconds |

Use proxy timeouts of at least 120 seconds; disable response buffering for `/connect`. The agent accepts only HTTPS URLs except localhost or 127.0.0.1 for development. Port mappings bind to `127.0.0.1` by default. If NPM runs in another container, connect it through a private network or set `JUMP_BIND_ADDRESS` to a private host IP and restrict access with a firewall. Never route the internal `/api/internal/*` endpoints from the public proxy: the shared internal bearer token is a second boundary, not a reason to expose them. Ideally block that path at NPM.

PostgreSQL and guacd have no host port mapping. Only Jump and broker need reverse proxy routes. Keep the Compose network private and do not expose database credentials.

## First run

Copy `.env.example` to `.env`; replace all placeholders. Use a URL-safe random hex database password. Generate the master key with `openssl rand -base64 32`. Set `COOKIE_SECURE=true` behind HTTPS. Run `docker compose up -d --build` and inspect `docker compose logs jump broker`. The Jump container runs `alembic upgrade head` before serving; subsequent starts repeat this safely. Open the UI and complete OIDC sign-in.

Build agents from `agent/`. Enroll with the one-time command displayed by the UI. Then run `jump-agent run` under a service manager with automatic restart. On Linux, keep the identity path at mode 0600 in a 0700 directory. On Windows, restrict the `%ProgramData%\Jump` directory to SYSTEM, Administrators and the agent service identity before enrollment. Example elevated command: `icacls "%ProgramData%\Jump" /inheritance:r /grant:r "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F"`. Confirm the service identity can read the file. Do not copy private identities between endpoints.

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

Backend: `cd app/backend && uv pip install -e '.[dev]' && alembic upgrade head && pytest && ruff check . && ruff format --check .`. Frontend: `cd app/frontend && npm install && npm test && npm run typecheck && npm run build`. Go: in `agent` and `broker`, run `go test ./... && go vet ./... && go build ./...`. Docker: `docker compose build && docker compose up -d`.
