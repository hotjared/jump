# Browser RDP (Phase 2)

An administrator chooses a saved Windows password credential in the device's **Remote** tab. Jump creates a one-use session bound to that administrator and device, then opens the desktop in the large session workspace. Closing the tab or browser attachment ends the session; there is no resume.

The browser speaks the Guacamole display protocol to Jump over a same-origin, authenticated WebSocket. Jump connects to the private `guacd:4822` service. guacd connects to a one-use listener on the Jump container's internal network address. Jump bridges that socket over a temporary, bounded stream in the existing broker and the agent's authenticated outbound WebSocket. The Windows agent dials **only `127.0.0.1:3389`**. There is no inbound RDP port or guacd host port to expose. The listener accepts one connection from the resolved guacd container address and closes after attachment.

The RDP stream requires the agent's `rdp_tunnel_v1` capability. An older agent advertising only `rdp` needs an update. The private broker verifies the exact session ID, Jump user, device ID, active agent connection ID, and capability with Jump before sending a `tcp_open` frame. Stream frames contain no host or port. Active sessions block agent updates. Revocation disconnects the agent and its streams. Device deletion waits for active sessions to close.

## Windows setup and credentials

Enable Remote Desktop on the Windows server and confirm its RDP service listens on `127.0.0.1:3389`. Save a `windows_password` credential on that device with a label, username, password, and optional domain. Only Jump administrators can manage credentials and start RDP. The password is encrypted with `JUMP_MASTER_KEY` at rest, decrypted only inside the Jump gateway for the guacd handshake, and never returned in browser API responses or written to audit events.

Jump sets guacd's RDP security mode to `any`, allowing guacd/FreeRDP to negotiate a protocol supported by the Windows RDP server. Modern Windows hosts may still negotiate NLA; automatic negotiation improves compatibility with hosts configured differently. TLS remains supported when negotiated, but `any` does not guarantee TLS or NLA for every server. Jump also requests display updates. The agent still connects only to `127.0.0.1:3389`.

guacd remains configured with `ignore-cert=true` because Windows hosts often use self-signed RDP certificates. **This skips verification of the Windows RDP server certificate when TLS is used.** Use a trusted server certificate and change this setting in code if certificate verification is required in your environment.

Clipboard copy and paste, file transfer, drive and printer redirection, audio, recording, unattended support links, VNC, and session resume are outside this phase. No RDP idle timer is enforced: Guacamole display traffic alone is not a reliable signal of user activity. Jump records the start, end or sanitized failure reason and periodically updates session activity metadata; it does not record screen contents or raw RDP bytes.
