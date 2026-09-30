"""Transient console-screen sessions using the existing agent broker."""

import asyncio
import base64
import json
import struct
import time
import uuid
from datetime import UTC, timedelta

from fastapi import APIRouter, Depends, HTTPException, Request, WebSocket, WebSocketDisconnect
from sqlalchemy import and_, or_, select, update
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session
from websockets.asyncio.client import connect as ws_connect
from websockets.exceptions import ConnectionClosed, InvalidHandshake

from .db import get_db
from .main import UPDATE_ACTIVE, admin, cfg, finish_remote_session, internal, latest_update
from .models import AuditEvent, Device, RemoteSession, User, now
from .screen_operations import (
    CLIPBOARD_WIRE_MAX,
    DESKTOPS,
    EVENTS,
    OPERATION_CODES,
    OPERATION_FIELDS,
    ScreenOperation,
)
from .session_diagnostics import record_stage

router = APIRouter()
ERRORS = {
    "device_offline": "Device is offline.",
    "unsupported_agent": "Update the Windows Jump service to enable Screen Control.",
    "no_interactive_session": "No physical Windows console session is available.",
    "helper_start_failed": "Could not start the Windows desktop helper.",
    "capture_failed": "Desktop capture failed.",
    "capture_invalid_dimensions": "Desktop capture failed.",
    "capture_get_dc_failed": "Desktop capture failed.",
    "capture_create_dc_failed": "Desktop capture failed.",
    "capture_create_bitmap_failed": "Desktop capture failed.",
    "capture_select_bitmap_failed": "Desktop capture failed.",
    "capture_stretch_mode_failed": "Desktop capture failed.",
    "capture_blit_failed": "Desktop capture failed.",
    "capture_flush_failed": "Desktop capture failed.",
    "capture_pixels_failed": "Desktop capture failed.",
    "capture_encode_failed": "Desktop capture failed.",
    "input_failed": "Windows input could not be delivered.",
    "interactive_session_changed": "The Windows console session changed. Start a new session.",
    # Retain the safe legacy reason for historical Diagnostics/older agents.
    # New helpers follow secure desktops and emit specific attachment failures.
    "secure_desktop": "Screen Control ended because Windows switched to a lock or secure desktop.",
    "desktop_open_failed": "Windows input desktop could not be opened.",
    "desktop_switch_failed": "Windows input desktop could not be attached.",
    "agent_disconnected": "Jump agent disconnected.",
    "session_timeout": "Screen session timed out.",
    "session_closed": "Screen session ended.",
    "browser_disconnected": "Browser disconnected.",
    "invalid_frame": "Invalid screen-session message.",
    "screen_busy": "A Screen Control session is already active.",
}
MAX_FRAME = 512 * 1024
CHUNK = 16384
SCREEN_LEASE_REFRESH_SECONDS = 30
SCREEN_LEASE_TIMEOUT = timedelta(minutes=2)


def eligible(device: Device | None) -> None:
    if not device or not device.online or not device.connection_id:
        raise HTTPException(409, "Device is offline")
    if not device.agent_identity or device.agent_identity.revoked_at:
        raise HTTPException(409, "Agent identity is revoked")
    if device.os_family != "windows":
        raise HTTPException(409, "Screen Control requires Windows")
    if "screen_control_v1" not in device.capabilities:
        raise HTTPException(409, ERRORS["unsupported_agent"])


def expire_stale_screen_sessions(db: Session, device: Device) -> None:
    cutoff = now() - SCREEN_LEASE_TIMEOUT
    stale = db.scalars(
        select(RemoteSession)
        .where(
            RemoteSession.device_id == device.id,
            RemoteSession.protocol == "screen",
            RemoteSession.state.in_(("connecting", "active")),
            or_(
                RemoteSession.connection_id.is_(None),
                RemoteSession.connection_id != device.connection_id,
                RemoteSession.last_activity_at < cutoff,
                # Preserve the shorter, existing window for unattached sessions.
                and_(
                    RemoteSession.state == "connecting",
                    RemoteSession.attached_at.is_(None),
                    RemoteSession.created_at < now() - timedelta(seconds=60),
                ),
            ),
        )
        .with_for_update()
        .execution_options(populate_existing=True)
    ).all()
    for session in stale:
        finish_remote_session(db, session, "session_timeout")


def refresh_screen_lease(db: Session, session_id: uuid.UUID, connection_id: str) -> bool:
    refreshed = db.execute(
        update(RemoteSession)
        .where(
            RemoteSession.id == session_id,
            RemoteSession.protocol == "screen",
            RemoteSession.state.in_(("connecting", "active")),
            RemoteSession.connection_id == connection_id,
        )
        .values(last_activity_at=now())
    )
    db.commit()
    return bool(refreshed.rowcount)


async def maintain_screen_lease(db: Session, session_id: uuid.UUID, connection_id: str) -> str:
    while True:
        await asyncio.sleep(SCREEN_LEASE_REFRESH_SECONDS)
        if not refresh_screen_lease(db, session_id, connection_id):
            return "session_timeout"


@router.post("/api/devices/{device_id}/screen-sessions", status_code=201)
def create_screen(
    device_id: uuid.UUID,
    request: Request,
    user: User = Depends(admin),
    db: Session = Depends(get_db),
):
    device = db.scalar(select(Device).where(Device.id == device_id).with_for_update())
    if not device:
        raise HTTPException(404)
    eligible(device)
    operation = latest_update(db, device.id)
    if operation and operation.state in UPDATE_ACTIVE:
        raise HTTPException(409, "Agent update in progress")
    expire_stale_screen_sessions(db, device)
    if db.scalar(
        select(RemoteSession.id)
        .where(
            RemoteSession.device_id == device_id,
            RemoteSession.protocol == "screen",
            RemoteSession.state.in_(("connecting", "active")),
        )
        .limit(1)
    ):
        raise HTTPException(409, ERRORS["screen_busy"])
    session = RemoteSession(
        id=uuid.uuid4(),
        device_id=device_id,
        device_name=device.display_name or device.hostname,
        user_id=user.id,
        protocol="screen",
        columns=1920,
        rows=1080,
        connection_id=device.connection_id,
        request_id=request.state.request_id,
    )
    db.add(session)
    try:
        db.flush()
        record_stage(db, session.id, "session_created")
        db.commit()
    except IntegrityError:
        db.rollback()
        raise HTTPException(409, ERRORS["screen_busy"]) from None
    return {"id": session.id, "state": session.state, "created_at": session.created_at}


@router.get("/api/internal/screen-streams/{session_id}/authorize", dependencies=[Depends(internal)])
def authorize_screen(
    session_id: uuid.UUID,
    device_id: uuid.UUID,
    connection_id: str,
    user_id: uuid.UUID,
    db: Session = Depends(get_db),
):
    session, device = db.get(RemoteSession, session_id), db.get(Device, device_id)
    user = db.get(User, user_id)
    if (
        not user
        or user.role != "admin"
        or not session
        or session.protocol != "screen"
        or session.device_id != device_id
        or session.user_id != user_id
        or session.state != "connecting"
        or not session.attached_at
        or session.connection_id != connection_id
    ):
        raise HTTPException(403, "Session unauthorized")
    try:
        eligible(device)
    except HTTPException:
        raise HTTPException(403, "Session unauthorized") from None
    if device.connection_id != connection_id:
        raise HTTPException(403, "Session unauthorized")
    return {"ok": True}


def valid_key(k):
    return type(k) is int and (
        48 <= k <= 57
        or 65 <= k <= 90
        or 112 <= k <= 135
        or 37 <= k <= 40
        or 160 <= k <= 165
        or k
        in (
            8,
            9,
            13,
            16,
            17,
            18,
            27,
            32,
            33,
            34,
            35,
            36,
            45,
            46,
            91,
            92,
            186,
            187,
            188,
            189,
            190,
            191,
            192,
            219,
            220,
            221,
            222,
        )
    )


def validate_input(value):
    if not isinstance(value, dict) or set(value) - {
        "action",
        "x",
        "y",
        "button",
        "key",
        "down",
        "delta",
    }:
        raise ValueError("invalid_frame")
    action = value.get("action")
    for field in ("x", "y", "button", "key", "delta"):
        if type(value.get(field, 0)) is not int:
            raise ValueError("invalid_frame")
    if type(value.get("down", False)) is not bool:
        raise ValueError("invalid_frame")
    x, y, button, key, delta = (value.get(k, 0) for k in ("x", "y", "button", "key", "delta"))
    down = value.get("down", False)
    valid = (
        0 <= x <= 65535
        and 0 <= y <= 65535
        and (
            action == "move"
            and not any((button, key, delta, down))
            or action == "button"
            and 0 <= button <= 2
            and not key
            and not delta
            or action == "wheel"
            and -1200 <= delta <= 1200
            and delta != 0
            and not any((button, key, down))
            or action == "key"
            and valid_key(key)
            and not any((x, y, button, delta))
            or action == "release"
            and not any((x, y, button, key, delta, down))
        )
    )
    if not valid:
        raise ValueError("invalid_frame")
    return value


def jpeg_size(data: bytes) -> tuple[int, int]:
    """Read the JPEG SOF header before allowing any browser image allocation."""
    if not data.startswith(b"\xff\xd8") or not data.endswith(b"\xff\xd9"):
        raise ValueError("invalid_frame")
    pos = 2
    while pos + 4 <= len(data):
        if data[pos] != 255:
            break
        marker = data[pos + 1]
        pos += 2
        if marker == 255:
            pos -= 1
            continue
        length = int.from_bytes(data[pos : pos + 2], "big")
        if length < 2 or pos + length > len(data):
            break
        if marker in (192, 193, 194):
            if length < 8:
                break
            height, width = struct.unpack("!HH", data[pos + 3 : pos + 7])
            if 1 <= width <= 1920 and 1 <= height <= 1080:
                return width, height
            break
        if marker == 218:
            break
        pos += length
    raise ValueError("invalid_frame")


class FrameAssembler:
    def __init__(self):
        self.last = self.current = self.index = self.count = 0
        self.width = self.height = 0
        self.data = bytearray()

    def add(self, frame):
        fields = ("frame_id", "index", "count", "width", "height")
        if any(type(frame.get(k, 0)) is not int for k in fields):
            raise ValueError("invalid_frame")
        fid, index, count, width, height = (frame.get(k, 0) for k in fields)
        data = frame.get("data")
        if (
            not isinstance(data, str)
            or len(data) > 21848
            or not 0 < fid < 2**53
            or not 1 <= count <= 32
            or not 0 <= index < count
            or not 1 <= width <= 1920
            or not 1 <= height <= 1080
        ):
            raise ValueError("invalid_frame")
        chunk = base64.b64decode(data, validate=True)
        if not 0 < len(chunk) <= CHUNK or index < count - 1 and len(chunk) != CHUNK:
            raise ValueError("invalid_frame")
        if index == 0:
            if self.current or fid <= self.last:
                raise ValueError("invalid_frame")
            self.current, self.index, self.count, self.width, self.height = (
                fid,
                0,
                count,
                width,
                height,
            )
            self.data.clear()
        if (fid, index, count, width, height) != (
            self.current,
            self.index,
            self.count,
            self.width,
            self.height,
        ):
            raise ValueError("invalid_frame")
        self.data.extend(chunk)
        self.index += 1
        if len(self.data) > MAX_FRAME:
            raise ValueError("invalid_frame")
        if self.index == self.count:
            data = bytes(self.data)
            self.data.clear()
            if jpeg_size(data) != (width, height):
                raise ValueError("invalid_frame")
            self.last, self.current = fid, 0
            return struct.pack("!QHH", fid, width, height) + data
        return None


@router.websocket("/ws/screen-sessions/{session_id}")
async def browser_screen(ws: WebSocket, session_id: uuid.UUID, db: Session = Depends(get_db)):
    if ws.headers.get("origin") != cfg.public_url.rstrip("/"):
        await ws.close(code=1008)
        return
    try:
        uid = uuid.UUID(ws.session.get("uid", ""))
    except (ValueError, TypeError):
        await ws.close(code=1008)
        return
    session, user = db.get(RemoteSession, session_id), db.get(User, uid)
    if (
        not user
        or user.role != "admin"
        or not session
        or session.user_id != uid
        or session.protocol != "screen"
        or session.state != "connecting"
    ):
        await ws.close(code=1008)
        return
    created = (
        session.created_at.replace(tzinfo=UTC)
        if session.created_at.tzinfo is None
        else session.created_at
    )
    if created < now() - timedelta(seconds=60):
        finish_remote_session(db, session, "session_timeout")
        await ws.close(code=1008)
        return
    claimed = db.execute(
        update(RemoteSession)
        .where(
            RemoteSession.id == session_id,
            RemoteSession.attached_at.is_(None),
            RemoteSession.state == "connecting",
        )
        .values(attached_at=now(), last_activity_at=now())
    )
    db.commit()
    if not claimed.rowcount:
        await ws.close(code=1008)
        return
    await ws.accept()
    record_stage(db, session.id, "browser_attached")
    db.commit()
    backend = None
    reason = "agent_disconnected"
    try:
        device = db.get(Device, session.device_id)
        eligible(device)
        if device.connection_id != session.connection_id:
            raise ValueError("agent_disconnected")
        address = (
            cfg.broker_internal_url.rstrip("/")
            .replace("http://", "ws://", 1)
            .replace("https://", "wss://", 1)
        )
        backend = await ws_connect(
            f"{address}/internal/screen-streams/{session.id}?device_id={device.id}&connection_id={session.connection_id}&user_id={uid}",
            additional_headers={"Authorization": "Bearer " + cfg.broker_internal_token},
            max_size=65536,
            open_timeout=10,
            close_timeout=2,
        )
        record_stage(db, session.id, "broker_connected")
        await backend.send(
            json.dumps({"version": 1, "type": "screen_open", "session_id": str(session.id)})
        )
        first = json.loads(await asyncio.wait_for(backend.recv(), 15))
        if first.get("version") != 1 or first.get("session_id") != str(session.id):
            raise ValueError("invalid_frame")
        if first.get("type") == "screen_error":
            raise ValueError(first.get("code") if first.get("code") in ERRORS else "capture_failed")
        if first.get("type") != "screen_opened":
            raise ValueError("invalid_frame")
        activated = db.execute(
            update(RemoteSession)
            .where(
                RemoteSession.id == session.id,
                RemoteSession.protocol == "screen",
                RemoteSession.state == "connecting",
                RemoteSession.connection_id == session.connection_id,
            )
            .values(state="active", connected_at=now(), last_activity_at=now())
        )
        if not activated.rowcount:
            db.rollback()
            raise ValueError("session_timeout")
        for stage in (
            "agent_session_opened",
            "interactive_session_found",
            "session_active",
        ):
            record_stage(db, session.id, stage)
        db.add(
            AuditEvent(
                event_type="screen_session_started",
                actor_user_id=uid,
                device_id=device.id,
                detail={"session_id": str(session.id)},
            )
        )
        db.commit()
        await ws.send_json({"type": "status", "state": "active"})
        # The acknowledged mode grants input only when no transition is pending.
        mode = "control"
        pending_mode = None
        operation = ScreenOperation()
        assembler = FrameAssembler()
        waiting = 0

        async def browser_to_agent():
            nonlocal mode, pending_mode, waiting
            start, events = time.monotonic(), 0
            while True:
                raw = await ws.receive_text()
                if len(raw.encode("utf-8")) > CLIPBOARD_WIRE_MAX:
                    raise ValueError("invalid_frame")
                frame = json.loads(raw)
                if not isinstance(frame, dict):
                    raise ValueError("invalid_frame")
                if time.monotonic() - start > 1:
                    start, events = time.monotonic(), 0
                events += 1
                if events > 250:
                    raise ValueError("invalid_frame")
                kind = frame.get("type")
                if kind != "screen_clipboard" and len(raw.encode("utf-8")) > 4096:
                    raise ValueError("invalid_frame")
                allowed = {
                    **OPERATION_FIELDS,
                    "screen_input": {"type", "input"},
                    "screen_mode": {"type", "mode"},
                    "screen_ack": {"type", "frame_id"},
                    "screen_close": {"type"},
                }
                if kind not in allowed or set(frame) != allowed[kind]:
                    raise ValueError("invalid_frame")
                if kind in OPERATION_FIELDS:
                    operation.request(frame)
                    if kind != "screen_operation_cancel" and (
                        mode != "control" or pending_mode is not None
                    ):
                        await ws.send_json(
                            {
                                "type": "screen_operation_result",
                                "request_id": operation.id,
                                "kind": operation.kind,
                                "code": "control_required",
                                "message": OPERATION_CODES["control_required"],
                            }
                        )
                        operation.clear()
                        continue
                elif kind == "screen_input":
                    validate_input(frame["input"])
                    if mode != "control" or pending_mode is not None:
                        continue
                elif kind == "screen_mode":
                    if frame["mode"] not in ("control", "view") or pending_mode is not None:
                        raise ValueError("invalid_frame")
                    pending_mode = frame["mode"]
                elif kind == "screen_ack":
                    if (
                        type(frame["frame_id"]) is not int
                        or not waiting
                        or frame["frame_id"] != waiting
                    ):
                        raise ValueError("invalid_frame")
                    waiting = 0
                else:
                    return "session_closed"
                frame.update(version=1, session_id=str(session.id))
                await backend.send(json.dumps(frame))

        async def agent_to_browser():
            nonlocal mode, pending_mode, waiting
            while True:
                frame = json.loads(await backend.recv())
                if frame.get("version") != 1 or frame.get("session_id") != str(session.id):
                    raise ValueError("invalid_frame")
                kind = frame.get("type")
                if kind == "screen_frame":
                    if waiting:
                        raise ValueError("invalid_frame")
                    complete = assembler.add(frame)
                    if complete:
                        if assembler.last == 1:
                            record_stage(db, session.id, "capture_started")
                            db.commit()
                        waiting = assembler.last
                        await asyncio.wait_for(ws.send_bytes(complete), 3)
                elif kind == "screen_event":
                    stage, desktop = frame.get("stage"), frame.get("desktop", "")
                    if (
                        stage not in EVENTS
                        or desktop not in DESKTOPS | {""}
                        or frame.get("data")
                        or desktop
                        and stage.startswith("sas_")
                    ):
                        raise ValueError("invalid_frame")
                    record_stage(db, session.id, stage)
                    db.commit()
                elif kind in (
                    "screen_clipboard",
                    "screen_clipboard_ack",
                    "screen_operation_result",
                ):
                    ignored = operation.cancelled and kind != "screen_operation_result"
                    operation.response(frame)
                    if ignored:
                        continue
                    if kind == "screen_operation_result":
                        await ws.send_json(
                            {
                                "type": kind,
                                "request_id": frame["request_id"],
                                "kind": frame["kind"],
                                "code": frame["code"],
                                "message": OPERATION_CODES[frame["code"]],
                            }
                        )
                    else:
                        fields = OPERATION_FIELDS[kind]
                        await ws.send_json(
                            {key: frame.get(key, 0 if key == "index" else "") for key in fields}
                        )
                elif kind == "screen_mode":
                    if pending_mode is None or frame.get("mode") != pending_mode:
                        raise ValueError("invalid_frame")
                    mode = pending_mode
                    pending_mode = None
                    await ws.send_json({"type": "screen_mode", "mode": mode})
                elif kind == "screen_error":
                    return frame.get("code") if frame.get("code") in ERRORS else "capture_failed"
                elif kind == "screen_close":
                    return "session_closed"
                else:
                    raise ValueError("invalid_frame")

        tasks = [
            asyncio.create_task(browser_to_agent()),
            asyncio.create_task(agent_to_browser()),
            asyncio.create_task(maintain_screen_lease(db, session.id, session.connection_id)),
        ]
        try:
            done, _ = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
            reason = next(iter(done)).result()
        finally:
            for task in tasks:
                task.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)
    except WebSocketDisconnect:
        reason = "browser_disconnected"
    except (ConnectionClosed, InvalidHandshake):
        reason = "agent_disconnected"
    except TimeoutError:
        reason = "session_timeout"
    except HTTPException:
        reason = "device_offline"
    except (ValueError, TypeError, KeyError, AttributeError, OSError) as exc:
        reason = str(exc) if str(exc) in ERRORS else "invalid_frame"
    finally:
        if backend:
            try:
                await backend.send(
                    json.dumps(
                        {"version": 1, "type": "screen_close", "session_id": str(session.id)}
                    )
                )
                await backend.close()
            except (ConnectionClosed, OSError):
                pass
        # A lost lease may have already been finalized by a creation request.
        # Reload before finalization to preserve that history and avoid duplicate audit events.
        db.refresh(session, with_for_update=True)
        finish_remote_session(db, session, reason)
        try:
            await ws.send_json(
                {
                    "type": "status",
                    "state": "closed",
                    "code": reason,
                    "message": ERRORS.get(reason, "Screen session ended."),
                }
            )
            await ws.close()
        except (RuntimeError, WebSocketDisconnect, OSError):
            pass
