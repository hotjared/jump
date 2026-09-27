import asyncio
import base64
import json
import logging
import uuid

import pytest
from starlette.websockets import WebSocketDisconnect

from jump import rdp
from jump.main import cfg


@pytest.mark.parametrize("ready_send_fails", [False, True])
def test_guacd_handshake_bridges_fixed_rdp_bytes_and_omits_redirection(
    monkeypatch, caplog, ready_send_fails
):
    caplog.set_level(logging.INFO, logger="jump.rdp")

    async def run():
        seen = {}

        async def guacd_server(reader, writer):
            _, selected = await rdp.read_instruction(reader)
            assert selected == ["select", "rdp"]
            # Stock guacamole/guacd:1.6.0 RDP argument ordering, captured
            # from the daemon's real args response in production.
            names = [
                "VERSION_1_5_0",
                "hostname",
                "port",
                "timeout",
                "domain",
                "username",
                "password",
                "width",
                "height",
                "dpi",
                "initial-program",
                "color-depth",
                "disable-audio",
                "enable-printing",
                "printer-name",
                "enable-drive",
                "drive-name",
                "drive-path",
                "create-drive-path",
                "disable-download",
                "disable-upload",
                "console",
                "console-audio",
                "server-layout",
                "security",
                "ignore-cert",
                "cert-tofu",
                "cert-fingerprints",
                "disable-auth",
                "remote-app",
                "remote-app-dir",
                "remote-app-args",
                "static-channels",
                "client-name",
                "enable-wallpaper",
                "enable-theming",
                "enable-font-smoothing",
                "enable-full-window-drag",
                "enable-desktop-composition",
                "enable-menu-animations",
                "disable-bitmap-caching",
                "disable-offscreen-caching",
                "disable-glyph-caching",
                "disable-gfx",
                "preconnection-id",
                "preconnection-blob",
                "timezone",
                "enable-sftp",
                "sftp-hostname",
                "sftp-host-key",
                "sftp-port",
                "sftp-timeout",
                "sftp-username",
                "sftp-password",
                "sftp-private-key",
                "sftp-passphrase",
                "sftp-public-key",
                "sftp-directory",
                "sftp-root-directory",
                "sftp-server-alive-interval",
                "sftp-disable-download",
                "sftp-disable-upload",
                "recording-path",
                "recording-name",
                "recording-exclude-output",
                "recording-exclude-mouse",
                "recording-exclude-touch",
                "recording-include-keys",
                "create-recording-path",
                "recording-write-existing",
                "resize-method",
                "enable-audio-input",
                "enable-touch",
                "read-only",
                "gateway-hostname",
                "gateway-port",
                "gateway-domain",
                "gateway-username",
                "gateway-password",
                "load-balance-info",
                "disable-copy",
                "disable-paste",
                "wol-send-packet",
                "wol-mac-addr",
                "wol-broadcast-addr",
                "wol-udp-port",
                "wol-wait-time",
                "force-lossless",
                "normalize-clipboard",
            ]
            writer.write(rdp.instruction("args", *names))
            await writer.drain()
            handshake = []
            while True:
                _, frame = await rdp.read_instruction(reader)
                handshake.append(frame)
                if frame[0] == "connect":
                    break
            assert ["size", "1280", "720", "96"] in handshake
            assert ["audio"] in handshake and ["video"] in handshake
            assert ["image", "image/png", "image/jpeg"] in handshake
            values = dict(zip(names, frame[1:], strict=True))
            assert frame[1] == "VERSION_1_5_0"
            assert frame[names.index("hostname") + 1] == values["hostname"]
            assert frame[names.index("port") + 1] == values["port"]
            assert frame[names.index("security") + 1] == "any"
            assert frame[names.index("ignore-cert") + 1] == "true"
            seen.update(values)
            tunnel_reader, tunnel_writer = await asyncio.open_connection(
                values["hostname"], int(values["port"])
            )
            tunnel_writer.write(b"rdp request")
            await tunnel_writer.drain()
            assert await tunnel_reader.readexactly(12) == b"rdp response"
            # The first connection closes the listener. A second connection
            # cannot attach to the session even while the first stays open.
            try:
                second_reader, second_writer = await asyncio.open_connection(
                    values["hostname"], int(values["port"])
                )
            except OSError:
                pass
            else:
                try:
                    assert await asyncio.wait_for(second_reader.read(1), timeout=1) == b""
                finally:
                    second_writer.close()
                    await second_writer.wait_closed()
            writer.write(rdp.instruction("ready", "opaque-id"))
            writer.write(rdp.instruction("sync", "1"))
            if not ready_send_fails:
                writer.write(rdp.instruction("img", "9", "15", "0", "image/png", "0", "0"))
                writer.write(
                    rdp.instruction("blob", "9", base64.b64encode(b"display bytes").decode())
                )
                writer.write(rdp.instruction("end", "9"))
                writer.write(rdp.instruction("clipboard", "7", "text/plain"))
                writer.write(
                    rdp.instruction("blob", "7", base64.b64encode(b"remote secret").decode())
                )
                writer.write(rdp.instruction("end", "7"))
            await writer.drain()
            if not ready_send_fails:
                received = [(await rdp.read_instruction(reader))[1] for _ in range(4)]
                assert received == [
                    ["ack", "7", "OK", "0"],
                    ["clipboard", "8", "text/plain"],
                    ["blob", "8", base64.b64encode(b"local secret").decode()],
                    ["end", "8"],
                ]  # The internal ping never reaches guacd.
                writer.write(rdp.instruction("ack", "8", "OK", "0"))
                await writer.drain()
                _, disconnected = await rdp.read_instruction(reader)
                assert disconnected == ["disconnect"]
            await reader.read()
            tunnel_writer.close()
            writer.close()

        server = await asyncio.start_server(guacd_server, "127.0.0.1", 0)
        monkeypatch.setattr(cfg, "guacd_host", "127.0.0.1")
        monkeypatch.setattr(cfg, "guacd_port", server.sockets[0].getsockname()[1])
        monkeypatch.setattr(cfg, "rdp_bridge_host", "127.0.0.1")
        loop = asyncio.get_running_loop()
        real_getaddrinfo = loop.getaddrinfo

        async def getaddrinfo(host, port, *args, **kwargs):
            addresses = await real_getaddrinfo(host, port, *args, **kwargs)
            if host == cfg.guacd_host and port is None:
                return [(*item[:4], ("192.0.2.55", *item[4][1:])) for item in addresses]
            return addresses

        # A DNS snapshot for the guacd service can differ from the source IP
        # of its connection. The bridge must still accept the first peer.
        monkeypatch.setattr(loop, "getaddrinfo", getaddrinfo)
        sid = str(uuid.uuid4())

        class Broker:
            def __init__(self):
                self.frames = asyncio.Queue()

            async def send(self, raw):
                frame = json.loads(raw)
                if frame["type"] == "tcp_open":
                    await self.frames.put(json.dumps({"type": "tcp_opened", "session_id": sid}))
                if frame["type"] == "tcp_data":
                    assert base64.b64decode(frame["data"]) == b"rdp request"
                    await self.frames.put(
                        json.dumps(
                            {
                                "type": "tcp_data",
                                "session_id": sid,
                                "data": base64.b64encode(b"rdp response").decode(),
                            }
                        )
                    )

            async def recv(self):
                return await self.frames.get()

            async def close(self):
                pass

        broker = Broker()

        async def connect(*args, **kwargs):
            assert "/internal/rdp-streams/" in args[0]
            assert kwargs["additional_headers"]["Authorization"].startswith("Bearer ")
            return broker

        monkeypatch.setattr(rdp, "ws_connect", connect)

        class Browser:
            def __init__(self):
                # Browser-provided endpoint hints must never reach guacd.
                self.query_params = {"hostname": "evil.example", "port": "3389"}
                self.sent = []
                self.ping_echoed = asyncio.Event()
                self.synced = asyncio.Event()
                self.remote_ended = asyncio.Event()
                self.local_ack_received = asyncio.Event()
                self.next_frame = 0

            async def send_text(self, raw):
                if ready_send_fails and rdp.parse_instruction(raw)[0] == "ready":
                    raise WebSocketDisconnect(code=1006)
                self.sent.append(raw)
                parts = rdp.parse_instruction(raw)
                if parts == ["", "ping", "12345"]:
                    self.ping_echoed.set()
                if parts == ["sync", "1"]:
                    self.synced.set()
                if parts == ["end", "7"]:
                    self.remote_ended.set()
                if parts == ["ack", "8", "OK", "0"]:
                    self.local_ack_received.set()

            async def receive_text(self):
                if not self.ping_echoed.is_set():
                    return rdp.instruction("", "ping", "12345").decode()
                await self.synced.wait()
                frames = [
                    rdp.instruction("ack", "7", "OK", "0"),
                    rdp.instruction("clipboard", "8", "text/plain"),
                    rdp.instruction("blob", "8", base64.b64encode(b"local secret").decode()),
                    rdp.instruction("end", "8"),
                    rdp.instruction("disconnect"),
                ]
                if self.next_frame == 0:
                    await self.remote_ended.wait()
                if self.next_frame == 4:
                    await self.local_ack_received.wait()
                frame = frames[self.next_frame]
                self.next_frame += 1
                return frame.decode()

        browser = Browser()
        started = []
        try:
            reason = await asyncio.wait_for(
                rdp.rdp_gateway(
                    browser,
                    sid,
                    str(uuid.uuid4()),
                    str(uuid.uuid4()),
                    str(uuid.uuid4()),
                    1280,
                    720,
                    96,
                    "Administrator",
                    "LAB",
                    b"secret-password",
                    lambda: started.append(True),
                    lambda: None,
                ),
                timeout=5,
            )
            assert reason == ("browser_disconnected" if ready_send_fails else "session_closed")
            assert started == [True]
            if ready_send_fails:
                assert browser.sent == []
                assert "first_close=browser_ready_send_failed" in caplog.text
                assert f"rdp browser ready send failed session={sid}" in caplog.text
            else:
                assert rdp.parse_instruction(browser.sent[0])[0] == "ready"
                assert ["", "ping", "12345"] in [rdp.parse_instruction(raw) for raw in browser.sent]
                assert ["sync", "1"] in [rdp.parse_instruction(raw) for raw in browser.sent]
                assert ["img", "9", "15", "0", "image/png", "0", "0"] in [
                    rdp.parse_instruction(raw) for raw in browser.sent
                ]
                assert ["blob", "9", base64.b64encode(b"display bytes").decode()] in [
                    rdp.parse_instruction(raw) for raw in browser.sent
                ]
                assert ["end", "9"] in [rdp.parse_instruction(raw) for raw in browser.sent]
                assert ["clipboard", "7", "text/plain"] in [
                    rdp.parse_instruction(raw) for raw in browser.sent
                ]
                assert ["blob", "7", base64.b64encode(b"remote secret").decode()] in [
                    rdp.parse_instruction(raw) for raw in browser.sent
                ]
            assert seen["username"] == "Administrator"
            assert seen["password"] == "secret-password" and seen["domain"] == "LAB"
            assert seen["security"] == "any" and seen["ignore-cert"] == "true"
            assert seen["hostname"] == "127.0.0.1"
            assert seen["port"] != browser.query_params["port"]
            assert "guacd_to_agent_bytes=11 agent_to_guacd_bytes=12" in caplog.text
            if not ready_send_fails:
                assert "first_close=browser_disconnect" in caplog.text
            assert "rdp bridge connection accepted" in caplog.text
            assert "peer_ip=127.0.0.1 resolved_guacd_ips=['192.0.2.55']" in caplog.text
            assert (
                f"rdp guacd connect parameters session={sid} hostname=127.0.0.1 "
                f"port={seen['port']} security=any ignore_cert=true"
            ) in caplog.text
            assert "secret-password" not in caplog.text
            assert "rdp request" not in caplog.text
            assert "local secret" not in caplog.text and "remote secret" not in caplog.text
            assert (
                seen["enable-drive"] == "false"
                and seen["disable-copy"] == "false"
                and seen["disable-paste"] == "false"
                and seen["disable-audio"] == "true"
                and seen["enable-printing"] == "false"
            )
        finally:
            server.close()
            await server.wait_closed()

    asyncio.run(run())


def test_guacd_error_categories_are_sanitized():
    assert (
        rdp.guacd_error_class(["error", "Server refused connection (wrong security type?)", "519"])
        == "transport"
    )
    assert (
        rdp.guacd_error_class(
            ["error", "SSL/TLS connection failed (untrusted/self-signed certificate?)", "519"]
        )
        == "tls"
    )
    assert (
        rdp.guacd_error_class(["error", "Authentication failure (invalid credentials?)", "769"])
        == "authentication"
    )
    assert rdp.guacd_error_class(["error", "password=hidden", "519"]) == "other"


def test_clipboard_streams_are_text_only_bounded_and_cleaned_up(caplog):
    streams = rdp.ClipboardStreams()
    assert streams.process(["clipboard", "1", "text/plain;charset=utf-8"]) == (True, False)
    payload = base64.b64encode(b"private clipboard text").decode()
    assert streams.process(["blob", "1", payload]) == (True, False)
    assert streams.process(["end", "1"]) == (True, False)
    assert not streams.active and streams.awaiting_ack == {"1"}
    assert "private clipboard text" not in caplog.text
    with pytest.raises(ValueError, match="untracked"):
        streams.process(["blob", "2", payload])
    with pytest.raises(ValueError, match="untracked"):
        streams.process(["end", "2"])
    for opcode in ("file", "pipe", "filesystem", "object", "body"):
        with pytest.raises(ValueError):
            streams.process([opcode, "3", "text/plain"])

    assert streams.process(["clipboard", "2", "image/png"]) == (False, True)
    assert streams.process(["blob", "2", payload]) == (False, False)
    assert streams.process(["end", "2"]) == (False, False)
    assert "2" not in streams.ignored
    assert streams.process(["clipboard", "3", "text/plain"]) == (True, False)
    assert streams.process(
        ["blob", "3", base64.b64encode(b"x" * rdp.CLIPBOARD_MAX_BYTES).decode()]
    ) == (True, False)
    assert streams.process(["blob", "3", base64.b64encode(b"y").decode()]) == (False, True)
    assert "3" not in streams.active
    assert streams.process(["end", "3"]) == (False, False)
    assert not streams.ignored
    assert streams.process(["clipboard", "4", "text/plain"]) == (True, False)
    assert streams.process(["blob", "4", base64.b64encode(b"\xff").decode()]) == (False, True)
    assert streams.process(["end", "4"]) == (False, False)
