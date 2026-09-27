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
            await writer.drain()
            if not ready_send_fails:
                _, received = await rdp.read_instruction(reader)
                assert received == ["disconnect"]  # The internal ping never reaches guacd.
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

            async def send_text(self, raw):
                if ready_send_fails and rdp.parse_instruction(raw)[0] == "ready":
                    raise WebSocketDisconnect(code=1006)
                self.sent.append(raw)
                parts = rdp.parse_instruction(raw)
                if parts == ["", "ping", "12345"]:
                    self.ping_echoed.set()
                if parts == ["sync", "1"]:
                    self.synced.set()

            async def receive_text(self):
                if not self.ping_echoed.is_set():
                    return rdp.instruction("", "ping", "12345").decode()
                await self.synced.wait()
                return rdp.instruction("disconnect").decode()

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


def test_clipboard_streams_are_directional_and_text_only(caplog):
    streams = rdp.ClipboardStreams()
    secret = "private clipboard value"
    blob = base64.b64encode(secret.encode()).decode()
    with caplog.at_level(logging.INFO, logger="jump.rdp"):
        assert streams.process(["blob", "1", blob], True) == (False, [])
        assert streams.process(["end", "1"], False) == (False, [])
        assert streams.process(["clipboard", "1", "image/png"], True)[0] is False
        assert streams.process(["clipboard", "1", "text/plain;charset=utf-8"], True) == (True, [])
        assert streams.process(["ack", "1", "OK", "0"], False) == (True, [])
        assert streams.process(["blob", "1", blob], False) == (False, [])
        assert streams.process(["blob", "1", blob], True) == (True, [])
        assert streams.process(["end", "1"], True) == (True, [])
        assert streams.local == {}
        assert streams.process(["blob", "1", blob], True) == (False, [])
        assert streams.process(["clipboard", "2", "text/plain"], False) == (True, [])
        assert streams.process(["ack", "2", "OK", "0"], True) == (True, [])
        assert streams.process(["blob", "2", blob], False) == (True, [])
        assert streams.process(["end", "2"], False) == (True, [])
        assert streams.remote == {}
    assert secret not in caplog.text


@pytest.mark.parametrize("from_browser", [True, False])
def test_clipboard_limit_and_invalid_payload_do_not_open_other_streams(from_browser):
    streams = rdp.ClipboardStreams()
    assert streams.process(["clipboard", "3", "text/plain"], from_browser) == (True, [])
    chunk = base64.b64encode(b"a" * rdp.CLIPBOARD_CHUNK_BYTES).decode()
    for _ in range(rdp.MAX_CLIPBOARD_BYTES // rdp.CLIPBOARD_CHUNK_BYTES):
        assert streams.process(["blob", "3", chunk], from_browser) == (True, [])
    allowed, reply = streams.process(["blob", "3", base64.b64encode(b"b").decode()], from_browser)
    assert not allowed and rdp.parse_instruction(reply[0].decode())[0] == "ack"
    assert streams.process(["blob", "3", chunk], from_browser) == (False, [])
    assert streams.process(["end", "3"], from_browser) == (False, [])
    assert not streams.local and not streams.remote
    assert streams.process(["clipboard", "4", "text/html"], from_browser)[0] is False
    assert streams.process(["file", "5", "text/plain", "name"], from_browser) == (False, [])
    for opcode in ("pipe", "filesystem", "object", "body", "get", "put"):
        assert streams.process([opcode, "5"], from_browser) == (False, [])
    assert streams.process(["clipboard", "5", "text/plain"], from_browser) == (True, [])
    assert streams.process(["blob", "5", "%%%"], from_browser)[0] is False
