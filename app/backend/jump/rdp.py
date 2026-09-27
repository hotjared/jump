"""Private guacd to agent bridge. No user-selected TCP destination is accepted."""

import asyncio
import base64
import codecs
import json
from collections.abc import Callable

from fastapi import WebSocket
from websockets.asyncio.client import connect as ws_connect

from .config import settings


def instruction(opcode: str, *args: str) -> bytes:
    return (",".join(f"{len(value)}.{value}" for value in (opcode, *args)) + ";").encode()


def parse_instruction(raw: str) -> list[str]:
    parts = []
    offset = 0
    while offset < len(raw):
        dot = raw.find(".", offset)
        if dot < 0 or dot - offset > 8 or not raw[offset:dot].isdigit():
            raise ValueError("invalid instruction")
        size = int(raw[offset:dot])
        if size > 65536 or dot + 1 + size > len(raw):
            raise ValueError("invalid instruction")
        end = dot + 1 + size
        parts.append(raw[dot + 1 : end])
        if end == len(raw) - 1 and raw[end] == ";":
            return parts
        if end >= len(raw) or raw[end] != ",":
            raise ValueError("invalid instruction")
        offset = end + 1
    raise ValueError("invalid instruction")


def split_instruction(buffer: str) -> tuple[str, list[str], str] | None:
    """Extract one length-prefixed Guacamole instruction from a TCP fragment."""
    offset = 0
    values = []
    while True:
        dot = buffer.find(".", offset)
        if dot < 0:
            if len(buffer) - offset > 8:
                raise ValueError("invalid instruction")
            return None
        if dot - offset > 8 or not buffer[offset:dot].isdigit():
            raise ValueError("invalid instruction")
        size = int(buffer[offset:dot])
        if size > 2_000_000:
            raise ValueError("instruction too large")
        end = dot + 1 + size
        if end >= len(buffer):
            return None
        values.append(buffer[dot + 1 : end])
        if buffer[end] == ";":
            return buffer[: end + 1], values, buffer[end + 1 :]
        if buffer[end] != ",":
            raise ValueError("invalid instruction")
        offset = end + 1


def guacd_failure(parts: list[str]) -> str:
    code = parts[2] if len(parts) > 2 else ""
    if code in {"769", "771"}:
        return "authentication_failed"
    if code in {"514", "522", "776"}:
        return "session_timeout"
    if code in {"519", "520"}:
        return "rdp_unavailable"
    return "guacd_disconnected"


async def read_instruction(reader: asyncio.StreamReader) -> tuple[str, list[str]]:
    # Handshake instructions are small and contain only ASCII parameter names.
    raw = await asyncio.wait_for(reader.readuntil(b";"), timeout=20)
    if len(raw) > 65536:
        raise ValueError("invalid handshake")
    value = raw.decode("utf-8")
    return value, parse_instruction(value)


async def rdp_gateway(
    browser: WebSocket,
    session_id: str,
    device_id: str,
    connection_id: str,
    user_id: str,
    width: int,
    height: int,
    dpi: int,
    username: str,
    domain: str | None,
    password: bytes,
    on_ready: Callable[[], None],
    on_activity: Callable[[], None],
) -> str:
    """Return a fixed reason code; never surface guacd's arbitrary error strings."""
    cfg = settings()
    broker = None
    guacd_writer = None
    listener = None
    bridge_task = None
    try:
        address = (
            cfg.broker_internal_url.rstrip("/")
            .replace("http://", "ws://", 1)
            .replace("https://", "wss://", 1)
        )
        broker = await ws_connect(
            f"{address}/internal/rdp-streams/{session_id}?device_id={device_id}&connection_id={connection_id}&user_id={user_id}",
            additional_headers={"Authorization": "Bearer " + cfg.broker_internal_token},
            max_size=65536,
            open_timeout=10,
            close_timeout=2,
        )
        await broker.send(json.dumps({"version": 1, "type": "tcp_open", "session_id": session_id}))
        first = json.loads(await asyncio.wait_for(broker.recv(), timeout=12))
        if first.get("session_id") != session_id or first.get("type") != "tcp_opened":
            return (
                first.get("code")
                if first.get("code") in {"rdp_unavailable", "unsupported_agent"}
                else "agent_disconnected"
            )

        accepted: asyncio.Future[tuple[asyncio.StreamReader, asyncio.StreamWriter]] = (
            asyncio.get_running_loop().create_future()
        )
        guacd_ips = {
            item[4][0]
            for item in await asyncio.get_running_loop().getaddrinfo(cfg.guacd_host, None)
        }

        async def inbound(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
            peer = writer.get_extra_info("peername")
            if accepted.done() or not peer or peer[0] not in guacd_ips:
                writer.close()
                return
            accepted.set_result((reader, writer))

        # Ephemeral listener lives only on the private Compose network. It
        # accepts one connection from the resolved guacd container address.
        listener = await asyncio.start_server(inbound, "0.0.0.0", 0)
        port = listener.sockets[0].getsockname()[1]

        async def bridge() -> None:
            reader, writer = await asyncio.wait_for(accepted, timeout=20)
            listener.close()

            async def from_guacd() -> None:
                while chunk := await reader.read(8192):
                    await broker.send(
                        json.dumps(
                            {
                                "version": 1,
                                "type": "tcp_data",
                                "session_id": session_id,
                                "data": base64.b64encode(chunk).decode(),
                            }
                        )
                    )

            async def from_agent() -> None:
                while True:
                    frame = json.loads(await broker.recv())
                    if frame.get("session_id") != session_id:
                        raise RuntimeError("agent_disconnected")
                    if frame.get("type") == "tcp_data":
                        chunk = base64.b64decode(frame.get("data", ""), validate=True)
                        if len(chunk) > 8192:
                            raise RuntimeError("agent_disconnected")
                        writer.write(chunk)
                        await writer.drain()
                    elif frame.get("type") == "tcp_close":
                        raise RuntimeError("agent_disconnected")
                    else:
                        raise RuntimeError("agent_disconnected")

            tasks = [asyncio.create_task(from_guacd()), asyncio.create_task(from_agent())]
            try:
                done, _ = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
                for task in done:
                    task.result()
            finally:
                for task in tasks:
                    task.cancel()
                await asyncio.gather(*tasks, return_exceptions=True)
                writer.close()
                await writer.wait_closed()

        bridge_task = asyncio.create_task(bridge())
        try:
            guacd_reader, guacd_writer = await asyncio.wait_for(
                asyncio.open_connection(cfg.guacd_host, cfg.guacd_port), timeout=5
            )
        except (OSError, TimeoutError):
            return "guacd_unavailable"
        guacd_writer.write(instruction("select", "rdp"))
        await guacd_writer.drain()
        _, args = await read_instruction(guacd_reader)
        if not args or args[0] != "args" or len(args) > 256:
            return "guacd_unavailable"
        options = {
            "hostname": cfg.rdp_bridge_host,
            "port": str(port),
            "username": username,
            "password": password.decode("utf-8"),
            "domain": domain or "",
            "security": "any",
            # Windows commonly generates a self-signed RDP certificate. When
            # TLS is negotiated, encryption remains enabled but the server
            # certificate is not verified.
            "ignore-cert": "true",
            "resize-method": "display-update",
            "disable-audio": "true",
            "enable-drive": "false",
            "disable-copy": "true",
            "disable-paste": "true",
            "enable-printing": "false",
        }
        guacd_writer.write(instruction("size", str(width), str(height), str(dpi)))
        guacd_writer.write(instruction("audio"))
        guacd_writer.write(instruction("video"))
        guacd_writer.write(instruction("image", "image/png", "image/jpeg"))
        guacd_writer.write(
            instruction("connect", args[1], *(options.get(name, "") for name in args[2:]))
        )
        await guacd_writer.drain()
        options["password"] = ""
        raw, first_guac = await read_instruction(guacd_reader)
        if first_guac[0] != "ready":
            return (
                guacd_failure(first_guac) if first_guac[0] == "error" else "authentication_failed"
            )
        on_ready()
        await browser.send_text(raw)

        async def browser_to_guacd() -> str:
            while True:
                raw = await browser.receive_text()
                if len(raw) > 1024:
                    return "browser_disconnected"
                parts = parse_instruction(raw)
                # Guacamole.WebSocketTunnel sends an internal stability ping as
                # soon as the socket opens (and periodically thereafter). It is
                # handled by the WebSocket gateway, never by guacd. Rejecting
                # it closes the session before the first display sync arrives.
                if parts[0] == "":
                    if len(parts) != 3 or parts[1] != "ping" or not parts[2].isdigit():
                        return "browser_disconnected"
                    await browser.send_text(raw)
                    continue
                if parts[0] not in {"key", "mouse", "size", "sync", "ack", "nop", "disconnect"}:
                    return "browser_disconnected"
                guacd_writer.write(raw.encode())
                await guacd_writer.drain()
                on_activity()
                if parts[0] == "disconnect":
                    return "session_closed"

        async def guacd_to_browser() -> str:
            decoder = codecs.getincrementaldecoder("utf-8")()
            pending = ""
            while True:
                try:
                    chunk = await asyncio.wait_for(guacd_reader.read(32768), timeout=10)
                except TimeoutError:
                    await browser.send_text(instruction("nop").decode())
                    continue
                if not chunk:
                    return "guacd_disconnected"
                pending += decoder.decode(chunk)
                if len(pending) > 2_100_000:
                    return "guacd_disconnected"
                while parsed := split_instruction(pending):
                    raw, parts, pending = parsed
                    if parts[0] == "error":
                        return guacd_failure(parts)
                    if parts[0] in {
                        "clipboard",
                        "file",
                        "pipe",
                        "filesystem",
                        "body",
                        "get",
                        "put",
                        "object",
                        "audio",
                        "video",
                        "require",
                    }:
                        return "guacd_disconnected"
                    if parts[0] not in {"log", "msg"}:
                        await browser.send_text(raw)
                        on_activity()

        tasks = [
            asyncio.create_task(browser_to_guacd()),
            asyncio.create_task(guacd_to_browser()),
            bridge_task,
        ]
        done, pending = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        result = next(iter(done))
        reason = result.result() if result != bridge_task else "agent_disconnected"
        for task in pending:
            task.cancel()
        await asyncio.gather(*pending, return_exceptions=True)
        return reason
    finally:
        if listener:
            listener.close()
            await listener.wait_closed()
        if bridge_task and not bridge_task.done():
            bridge_task.cancel()
            await asyncio.gather(bridge_task, return_exceptions=True)
        if guacd_writer:
            guacd_writer.close()
            await guacd_writer.wait_closed()
        if broker:
            try:
                await broker.send(
                    json.dumps({"version": 1, "type": "tcp_close", "session_id": session_id})
                )
            except Exception:
                pass
            await broker.close()
