# Architecture

```mermaid
flowchart TD
    Browser["Browser"] --> Proxy["TLS reverse proxy"]
    Agent["Jump agent"] --> Proxy
    Proxy --> API["Jump UI / API"]
    Proxy --> Broker["Go broker"]
    Broker --> API
    API --> DB["PostgreSQL 18"]
    API -. "Phase 2" .-> Guacd["guacd"]
```

## Trust flow

The browser authenticates with a standards-based OIDC provider using authorization code flow and state/nonce validation through Authlib. Jump maps the pair `(issuer, subject)` to a local user. The first authenticated user becomes admin under a serialized database lock; later users get the user role. Privileged mutations check the admin role on the API. An HTTP-only, SameSite=Lax, Secure cookie holds a signed session identifier; mutating browser requests require an exact configured Origin and session-bound CSRF token. The identity provider owns MFA policy.

An admin creates a 256-bit random enrollment token. PostgreSQL stores only its SHA-256 digest, expiry, creator and redemption state. The browser displays plaintext once. The agent generates an Ed25519 keypair **before** enrollment, sends only its public key and token over HTTPS, and stores the private key locally. An atomic conditional database update consumes the token, creates a stable device UUID, and records the public key in one transaction.

For each new WebSocket connection, the broker fetches the active public key from the private API, sends a fresh 32-byte random challenge, and verifies a signature over `jump-agent-v1:` plus the challenge. It never reuses a nonce. A successful connection gets a unique connection ID. The API accepts heartbeat and disconnect only for the current ID, so an old connection cannot mark a replacement offline. Revoking an agent identity under the admin session records the actor and device, permanently rejects its public key, clears presence, and calls a separate authenticated private broker listener to close the active WebSocket. The connection registration and broker disconnect are serialized, while the API locks the device and identity rows so an in-flight handshake cannot revive a revoked device. Revocation retains the device record and audit history; deletion is a separate operation. A failed broker callback returns 503 after the revocation is committed, so an admin can retry the idempotent action; subsequent heartbeats and reconnects are still denied. The broker reconciles online flags on startup. It pings every 25 seconds and closes idle connections after 75 seconds; the agent heartbeats every 20 seconds and reconnects with capped exponential backoff and jitter.

The V1 JSON envelope has a mandatory `version` and `type`. Messages are capped at 16 KiB. Only `challenge`, `auth`, `ready`, and `heartbeat` exist. Future protocol versions can add typed stream open/data/close frames with stream IDs, flow control and per-operation authorization. Never treat the present connection as permission to perform endpoint actions.

## Data and credential boundary

Users, devices, group membership, many-to-many tags, enrollment tokens, Ed25519 public identities, credentials, and general audit events live in PostgreSQL. Devices use a UUID independent of hostname. Credential secrets are encrypted in the API with AES-256-GCM, a 96-bit random nonce, version number, and credential-ID authenticated associated data. `JUMP_MASTER_KEY` is external and never goes into PostgreSQL. The UI and device APIs have no endpoint for decrypted secrets. SSH session startup decrypts only the selected credential and sends it to the agent for that session. Future rotation will decrypt each record under its stored version and re-encrypt with the new key. Keep retired keys until rotation completes.

## Future session gateway

- RDP: browser → Jump session gateway → guacd → broker temporary reverse stream → persistent agent → `127.0.0.1:3389` on Windows.
- SSH: browser → Jump session gateway → broker logical session over the persistent agent connection → agent SSH client → `127.0.0.1:22` on Linux.
- Files: browser → Jump API → broker → agent → local filesystem. Planned operations: list, upload, download, rename, delete, create directory. This is separate from Guacamole drive redirection and SFTP.

The agent initiates outbound TLS connections. Endpoints need no inbound Internet ports.

## Roadmap

- **Phase 2:** browser SSH is implemented over multiplexed logical agent sessions. RDP, file transfer, and remote actions remain future work.
- **Phase 3:** cross-platform files, shell/PowerShell, reboot and restart-agent actions, richer system information.
- **Screen Control v1:** Windows console capture/control uses an interactive helper launched by the enrolled LocalSystem service. Attended Quick Support and consent workflows remain future work.

No VNC, session recording, multitenancy, monitoring, patching, alerting or billing is planned for this foundation.

## Known limitations

- No RDP or file operations yet; those capability names are descriptive.
- `current_user` reports the agent process account where available, not a detected interactive desktop session.
- No credential management API or UI yet. The encrypted model and service functions are in place.
- No internal device certificate authority yet. Ed25519 challenge/response can later be replaced by short-lived device certificates, keeping the same public identity and enrollment boundary.
- Presence is held by one broker process. Scaling to multiple replicas needs shared coordination and routing; run one broker for now.
- OIDC provider and its access policy must be configured by the operator. There is no local login or recovery login.
## SSH session path

Browser → authenticated Jump WebSocket → private token-authenticated broker
session channel → existing Ed25519-authenticated agent WebSocket → agent SSH
client → `127.0.0.1:22`.

The broker multiplexes session IDs over the one agent connection while
presence and heartbeats continue. Each browser session is owned by a Jump user
and has a database row for creation, activity, connection, and close state.
Jump sends only the selected decrypted credential in a single `session_open`
message to that agent. The agent reports the host-key fingerprint after SSH
authentication; Jump pins it on first success and rejects a later mismatch.
Terminal bytes are forwarded with bounded frames and are never persisted.

## Screen Control session path

Browser → authenticated Jump WebSocket → private bearer-authenticated broker screen route → existing Ed25519-authenticated outbound agent connection → private local named pipe → interactive desktop helper.

Screen Control uses `RemoteSession.protocol = "screen"`, the existing workspace, session Diagnostics and Audit Log. Session creation requires an administrator, an online Windows device, a non-revoked identity and `screen_control_v1` or `screen_control_v2`. The session snapshots the device connection ID and reserves one controller per device with a partial unique database index. Browser attachment is one-shot; the broker rechecks ownership and the exact connection through the backend before routing.

The service discovers the physical console session using `WTSGetActiveConsoleSessionId` and validates its WTS state (Active, Connected or Init). No logged-on user token is needed, including before sign-in. It duplicates its LocalSystem primary token, sets its session ID, and launches the protected installed `jump-agent.exe internal-desktop-helper` on `winsta0\default`. The helper is privileged so ordinary elevated application windows can receive input where Windows permits it. It never loads enrollment identity, receives server credentials or opens a network connection. Its environment contains only SystemRoot. A service-owned kill-on-close Windows Job Object prevents orphan helpers, including after a service crash.

IPC uses a randomly named, first-instance named pipe with a protected SYSTEM-only ACL, remote clients rejected and both peers checking process IDs. No local-resource selector is accepted from browser frames. Helper messages are length-prefixed and bounded. GDI capture and SendInput run on a fixed helper thread. The active console ID and input-desktop name are checked before capture/input. The window-free capture/input thread follows `OpenInputDesktop` with read/write-object rights and `SetThreadDesktop`; it never calls `SwitchDesktop`. It releases held input, attaches the replacement, then closes its previously owned desktop handle. Desktop discovery/attachment/capture gaps are retried for at most five seconds. If Windows replaces the console session, the service replaces only the helper with a bounded relaunch window; the Screen route, mode and frame counter remain intact. No desktop ACLs or UAC policy are changed.

Frames are JPEG, primary display only, scaled to fit 1920×1080. Capture checks run approximately six times per second; unchanged encoded images are suppressed. Each frame is at most 512 KiB and split into at most 32 chunks of 16 KiB over the unchanged 64 KiB agent-message limit. The broker has a 32-message screen queue and rejects invalid sequences on that route only. The backend validates ordering, cumulative size and JPEG dimensions before sending a transient binary frame to the browser. One rendered-frame acknowledgement grants credit for the next frame; absent acknowledgement closes only the screen session after five seconds. Browser/internal input is bounded to 4 KiB messages and 250 messages per second, with a 64-entry agent input queue.

Control/View Only is enforced independently in the frontend, backend, broker and agent. Switching modes releases held keys/buttons; Control resumes only after the helper-side mode acknowledgement. Keyboard listeners belong to the screen canvas and require the active session, rather than capturing page-global keyboard input. Screen images, input events and IPC names are never written to storage or diagnostics.


### Windows Screen capture diagnostics

The physical console helper captures into a bounded, top-down 32-bit BI_RGB
`CreateDIBSection` surface. It checks bitmap selection, configures HALFTONE
with the required brush origin, and tries `SRCCOPY | CAPTUREBLT` followed by
at most one `SRCCOPY` fallback. Unscaled captures use `BitBlt`; scaled
captures use `StretchBlt`. Cursor overlay is best effort. `GdiFlush`
completes drawing before the helper copies BGRX pixels into opaque RGBA.
Cleanup restores the previous bitmap, destroys the memory DC before deleting
the DIB, and releases the borrowed screen DC on success and every failure path.

The existing adaptive JPEG encoder, 512 KiB frame limit, frame chunks and
acknowledgement flow are unchanged. Capture failures propagate only these
allowlisted reasons through Screen IPC, Diagnostics and Audit Log:
`capture_invalid_dimensions`, `capture_get_dc_failed`,
`capture_create_dc_failed`, `capture_create_bitmap_failed`,
`capture_select_bitmap_failed`, `capture_stretch_mode_failed`,
`capture_blit_failed`, `capture_flush_failed`, `capture_pixels_failed`, and
`capture_encode_failed`. The browser message remains “Desktop capture
failed.” Pixel buffers, screenshots, handles, pointers, paths and raw Windows
errors are never included in these diagnostics.

Installed-service verification requires a newly built Windows amd64 agent:
log into the physical console, start Screen Control, and confirm the first
frame produces `capture_started`. If it fails, report the allowlisted
failure reason from Diagnostics. Verify disconnect releases the helper,
agent presence remains online, and RDP/Files still work. The same session must also survive sign-in, lock/unlock, UAC and logoff, as described below.

API requirements: [CreateDIBSection](https://learn.microsoft.com/en-us/windows/win32/api/wingdi/nf-wingdi-createdibsection)
and [StretchBlt](https://learn.microsoft.com/en-us/windows/win32/api/wingdi/nf-wingdi-stretchblt).


### Unattended Windows console, SAS and clipboard

This functionality is for administrator access to permanently enrolled Windows
amd64 devices running the installed LocalSystem JumpAgent service. It follows
Windows' physically active WinSta0 desktop, including sign-in/Winlogon, Default,
lock, ScreenSaver and UAC. Internal OS desktop names may guide attachment, but
only the fixed names Default, Winlogon and ScreenSaver are allowed in transient
protocol events. Persisted Diagnostics contain only deduplicated stages:
`desktop_attached`, `desktop_changed`, `sas_requested`, `sas_sent`. Persistent
failure reasons `desktop_open_failed` and `desktop_switch_failed` contain no
raw Windows errors, names, handles or desktop contents.

The workspace has explicit **Paste to Remote**, **Copy from Remote** and
**Ctrl+Alt+Del** actions. All require acknowledged Control mode and no pending
mode transition, independently in browser, backend, broker and agent. View Only
cannot read/write the clipboard or request SAS. A mode change immediately
revokes input and cancels an in-flight operation; queued replies are drained
without ending the Screen stream. Only one operation is active at a time.

SAS is a dedicated `screen_operation` of kind `sas`, never a Control/Alt/Delete
SendInput sequence. The service duplicates its own SYSTEM token into the Screen
session's physical console session, impersonates it on a locked OS thread,
invokes `SendSAS(FALSE)`, and reverts impersonation. Windows `SendSAS` returns
void; `sas_sent` and the browser's "request sent" feedback mean submitted,
not confirmation that Windows displayed a SAS screen.

Administrators must configure **Computer Configuration → Administrative
Templates → Windows Components → Windows Logon Options → Disable or enable
software Secure Attention Sequence**, enabled with **Services** or **Services
and Ease of Access applications**. Jump reads the corresponding
`HKLM\Software\Microsoft\Windows\CurrentVersion\Policies\System\SoftwareSASGeneration`
DWORD (1 or 3), and never writes policy. Missing, disabled, unreadable or
non-service policy returns `sas_blocked`: "Windows policy blocked remote
Ctrl+Alt+Del." It leaves Screen Control and agent presence online. An unavailable
SAS API/token returns `sas_unavailable`. See Microsoft's
[SendSAS contract](https://learn.microsoft.com/en-us/windows/win32/api/sas/nf-sas-sendsas)
and [SoftwareSASGeneration policy](https://learn.microsoft.com/en-us/windows/client-management/mdm/policy-csp-admx-winlogon#softwaresasgeneration).

Clipboard is plain text only, explicitly requested, with no synchronization.
The browser reads/writes `navigator.clipboard`; the helper uses CF_UNICODETEXT
and a message-only clipboard owner on a separate locked OS thread. Clipboard
work never creates a window/hook on the capture/input thread. No delayed
rendering or clipboard listener is installed. Clipboard text, screenshots and
input are never persisted in Diagnostics, Audit Log or application logs.

`screen_operation` identifies a UUID request and kind `clipboard_set` or
`clipboard_get`. `screen_clipboard` carries ordered 16 KiB base64-encoded UTF-8
chunks, at most 64 chunks and 1 MiB total. An empty clipboard is one empty
chunk. `screen_clipboard_ack` credits exactly one chunk before the next one;
`screen_operation_result` contains only an allowlisted status, and
`screen_operation_cancel` cancels the request. UTF-8 is validated after assembly
(including code points split across chunks); embedded NUL is rejected. Request
IDs, sequence, counts, credit and total bytes are checked independently on each
transport hop. Clipboard messages alone receive a 24 KiB Screen wire allowance;
ordinary input remains limited to 4 KiB and the agent WebSocket limit stays
64 KiB. Frame size/chunks/ACK and the single-controller database index are
unchanged. Operations time out after 30 seconds; malformed traffic closes only
the Screen route, while clipboard/SAS availability/policy errors are nonfatal
operation statuses. IPC drains independently of frame credit, retaining only
one latest unconsumed capture, so clipboard replies cannot stall behind a frame.

Keep this PR draft until testing a new installed LocalSystem agent on Windows
Server, Windows 10 and Windows 11 (as available) confirms the **same browser
Screen session** throughout this physical-console flow:

1. Start while logged out and see the sign-in screen and `capture_started`.
2. Invoke Ctrl+Alt+Del with service SAS policy enabled; separately verify the
   useful blocked-policy status with policy disabled/unconfigured.
3. Enter credentials and reach Default without reconnecting.
4. Lock, invoke SAS, unlock and return to the same session.
5. Trigger UAC on its secure desktop, control the prompt and return to Default.
6. Explicitly paste/copy Unicode, empty text and 1 MiB text; verify clipboard
   permission errors and View Only rejection without automatic synchronization.
7. Log off and see sign-in on the existing session. Confirm continuing frames,
   held-input release, stable mode and one controller across all transitions.
8. Confirm agent presence remains online, and Files/RDP/SSH still function.

No Support Links, temporary agents, attended consent, recording, audio,
additional monitors, drag/drop or policy modification are included.

### Screen protocol rolling upgrades

Supported Windows service agents advertise both `screen_control_v1` and
`screen_control_v2`. The unattended-console operations and events use v2.
Capability decisions use advertised metadata, never agent version strings.
A v1-only agent retains basic capture/input, mode switching, Files, Fullscreen
and Disconnect. The workspace hides clipboard/SAS actions and explains that
updating the Windows agent enables unattended admin controls.

The backend binds v2 support to the session's current connection metadata.
The broker independently checks the authenticated connection's capabilities
and snapshots support onto its route. Both suppress v2-only browser traffic
to a v1 agent and return a safe `unsupported_agent` operation result, without
closing the basic Screen session or agent presence.

New servers request `screen_version: 2` in `screen_open` only for v2 agents.
The broker forwards that negotiation only when the connected agent supports
v2. The envelope remains `version: 1`; frames/input/modes are unchanged.
Without explicit v2 negotiation, the new agent suppresses v2 operations and
drains desktop events without sending them to the server. Thus an older server
that only checks v1 still receives the basic protocol it understands.
