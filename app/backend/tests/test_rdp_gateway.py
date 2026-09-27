import asyncio
import base64
import json
import uuid

from jump import rdp
from jump.main import cfg


def test_guacd_handshake_bridges_fixed_rdp_bytes_and_omits_redirection(monkeypatch):
    async def run():
        seen = {}

        async def guacd_server(reader, writer):
            _, selected = await rdp.read_instruction(reader)
            assert selected == ["select", "rdp"]
            names = [
                "VERSION_1_5_0",
                "hostname",
                "port",
                "username",
                "password",
                "domain",
                "security",
                "ignore-cert",
                "enable-drive",
                "disable-copy",
                "disable-paste",
                "disable-audio",
                "enable-printing",
                "resize-method",
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
            seen.update(values)
            tunnel_reader, tunnel_writer = await asyncio.open_connection(
                values["hostname"], int(values["port"])
            )
            tunnel_writer.write(b"rdp request")
            await tunnel_writer.drain()
            assert await tunnel_reader.readexactly(12) == b"rdp response"
            writer.write(rdp.instruction("ready", "opaque-id"))
            writer.write(rdp.instruction("sync", "1"))
            await writer.drain()
            _, received = await rdp.read_instruction(reader)
            assert received == ["disconnect"]  # The internal ping never reaches guacd.
            await reader.read()
            tunnel_writer.close()
            writer.close()

        server = await asyncio.start_server(guacd_server, "127.0.0.1", 0)
        monkeypatch.setattr(cfg, "guacd_host", "127.0.0.1")
        monkeypatch.setattr(cfg, "guacd_port", server.sockets[0].getsockname()[1])
        monkeypatch.setattr(cfg, "rdp_bridge_host", "127.0.0.1")
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
                self.sent = []
                self.ping_echoed = asyncio.Event()
                self.synced = asyncio.Event()

            async def send_text(self, raw):
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
            assert reason == "session_closed" and started == [True]
            assert rdp.parse_instruction(browser.sent[0])[0] == "ready"
            assert ["", "ping", "12345"] in [rdp.parse_instruction(raw) for raw in browser.sent]
            assert ["sync", "1"] in [rdp.parse_instruction(raw) for raw in browser.sent]
            assert seen["username"] == "Administrator"
            assert seen["password"] == "secret-password" and seen["domain"] == "LAB"
            assert seen["security"] == "any" and seen["ignore-cert"] == "true"
            assert (
                seen["enable-drive"] == "false"
                and seen["disable-copy"] == "true"
                and seen["disable-paste"] == "true"
                and seen["disable-audio"] == "true"
                and seen["enable-printing"] == "false"
            )
        finally:
            server.close()
            await server.wait_closed()

    asyncio.run(run())
