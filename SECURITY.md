# Security policy

## Supported versions

The latest release on `main` receives security fixes. This repository is currently a pre-release foundation; do not assume unattended Internet exposure has been audited.

## Reporting

Please report vulnerabilities privately through GitHub's **Report a vulnerability** / private security advisory for this repository. Do not include working exploits or secrets in public issues. Include affected version, impact and reproduction steps. Maintainers will coordinate a fix and disclosure.

## Boundaries and threats

OIDC authenticates humans; Jump stores local subject-to-role mapping. Browser sessions use signed secure HTTP-only cookies and same-origin CSRF checks. Admin operations enforce RBAC in the API. A one-time hashed enrollment token authorizes the creation of an asymmetric agent identity; future connections prove possession of the private key against fresh broker challenges. The broker reaches the API over a private Compose network using an independent high-entropy bearer token. The UI never gets credential plaintext.

The reverse proxy terminates TLS. Protect traffic between the proxy and private services, restrict internal API routes, and do not publish guacd or PostgreSQL. An attacker with the agent private key can impersonate that device until its identity is revoked; protect agent state files and replace compromised devices. A compromised database does not reveal credential plaintext without the separate master key, but it does expose metadata and audit history. A compromised master key plus database exposes credentials.

Enrollment token guessing and unauthenticated WebSocket handshakes need deployment-level rate limiting at the reverse proxy; the application validates bounded messages and tokens. OIDC login attempts should also be rate limited at the proxy and provider. Logs must not include token plaintext, private keys, passwords, cookies, OIDC secrets or the master key.

## Secret management

Use independent 32-byte random values for session and broker secrets, and base64 of 32 random bytes for `JUMP_MASTER_KEY`. Keep `.env` out of source control, with restrictive filesystem permissions. Back up the master key separately from database backups. Rotate OIDC and broker secrets if exposed. Credential key rotation requires a migration process using stored `key_version`; changing the master key alone makes existing credentials unreadable.
