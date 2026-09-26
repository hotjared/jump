import asyncio
import base64
import binascii
import hmac
import json
import logging
import re
import secrets
import time
import uuid
from datetime import UTC, timedelta
from pathlib import Path

import httpx
from authlib.integrations.starlette_client import OAuth
from fastapi import (
    Depends,
    FastAPI,
    Header,
    HTTPException,
    Request,
    Response,
    WebSocket,
    WebSocketDisconnect,
)
from fastapi.responses import FileResponse, RedirectResponse
from fastapi.staticfiles import StaticFiles
from sqlalchemy import func, select, update
from sqlalchemy.orm import Session
from starlette.middleware.sessions import SessionMiddleware
from starlette.middleware.trustedhost import TrustedHostMiddleware
from websockets.asyncio.client import connect as ws_connect
from websockets.exceptions import ConnectionClosed

from .config import settings
from .credentials import create_credential, decrypt_for_gateway
from .db import get_db
from .models import (
    AgentIdentity,
    AgentUpdate,
    AuditEvent,
    Credential,
    Device,
    EnrollmentToken,
    Group,
    RemoteSession,
    Tag,
    User,
    now,
)
from .releases import agent_downloads, newer_release, release_asset, release_number
from .schemas import (
    CredentialInput,
    DevicePatch,
    EnrollmentInput,
    EnrollRequest,
    Metadata,
    NameInput,
    PresenceInput,
    SSHSessionInput,
)
from .security import consume_enrollment, create_enrollment, map_oidc_user, require_admin

log = logging.getLogger("jump")
logging.basicConfig(level=logging.INFO, format="%(message)s")
cfg = settings()
cfg.validate_production()
app = FastAPI(title="Jump", docs_url=None, redoc_url=None)
oauth = OAuth()
oauth.register(
    name="provider",
    client_id=cfg.oidc_client_id,
    client_secret=cfg.oidc_client_secret,
    server_metadata_url=cfg.oidc_issuer.rstrip("/") + "/.well-known/openid-configuration",
    client_kwargs={"scope": "openid profile email"},
)


@app.middleware("http")
async def request_guard(request: Request, call_next):
    request.state.request_id = str(uuid.uuid4())
    started = time.perf_counter()
    response = None
    if request.url.path.startswith("/api/") and request.method not in ("GET", "HEAD", "OPTIONS"):
        # Browser session writes require a same-origin request plus a double-submit CSRF header.
        if not request.url.path.startswith("/api/internal/"):
            origin = request.headers.get("origin")
            if origin != cfg.public_url.rstrip("/"):
                response = Response("Invalid Origin", status_code=403)
            elif not (expected := request.session.get("csrf")) or not hmac.compare_digest(
                request.headers.get("x-csrf-token", ""), expected
            ):
                response = Response("Invalid CSRF token", status_code=403)
    if response is None:
        response = await call_next(request)
    response.headers["X-Request-ID"] = request.state.request_id
    response.headers["Cache-Control"] = (
        "no-store" if request.url.path.startswith("/api/") else "public"
    )
    response.headers["X-Content-Type-Options"] = "nosniff"
    response.headers["Content-Security-Policy"] = (
        "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'"
    )
    log.info(
        json.dumps(
            {
                "event": "http_request",
                "request_id": request.state.request_id,
                "user_id": request.session.get("uid"),
                "method": request.method,
                "path": request.url.path,
                "status": response.status_code,
                "duration_ms": round((time.perf_counter() - started) * 1000, 1),
            }
        )
    )
    return response


# Session middleware must wrap the CSRF middleware so request.session is available.
app.add_middleware(
    SessionMiddleware,
    secret_key=cfg.session_secret,
    https_only=cfg.cookie_secure,
    same_site="lax",
    max_age=8 * 3600,
    session_cookie="jump_session",
)
app.add_middleware(TrustedHostMiddleware, allowed_hosts=cfg.allowed_hosts.split(","))


def current_user(request: Request, db: Session = Depends(get_db)) -> User:
    raw = request.session.get("uid")
    try:
        user = db.get(User, uuid.UUID(raw)) if raw else None
    except ValueError:
        user = None
    if not user:
        raise HTTPException(401, "Login required")
    return user


def admin(user: User = Depends(current_user)) -> User:
    require_admin(user)
    return user


def internal(authorization: str = Header(default="")) -> None:
    expected = "Bearer " + cfg.broker_internal_token
    if not hmac.compare_digest(authorization, expected):
        raise HTTPException(401, "Unauthorized")


UPDATE_ACTIVE = ("pending", "downloading", "installing", "restarting")
UPDATE_REASONS = {
    "download_failed",
    "checksum_mismatch",
    "install_failed",
    "rollback_completed",
    "reconnect_timeout",
    "agent_unavailable",
    "service_restarted",
}


def update_output(op: AgentUpdate) -> dict:
    return {
        "id": str(op.id),
        "from_version": op.from_version,
        "target_version": op.target_version,
        "state": op.state,
        "created_at": op.created_at,
        "started_at": op.started_at,
        "restarting_at": op.restarting_at,
        "completed_at": op.completed_at,
        "failure_reason": op.failure_reason,
    }


def finish_update(db: Session, op: AgentUpdate, reason: str | None = None) -> None:
    if op.state not in UPDATE_ACTIVE:
        return
    op.state = "failed" if reason else "completed"
    op.failure_reason = reason
    op.completed_at = now()
    db.add(
        AuditEvent(
            event_type="agent_update_failed" if reason else "agent_update_completed",
            actor_user_id=op.actor_user_id,
            device_id=op.device_id,
            detail={
                "operation_id": str(op.id),
                "from_version": op.from_version,
                "target_version": op.target_version,
                "reason": reason,
            },
        )
    )
    db.commit()


def latest_update(db: Session, device_id: uuid.UUID) -> AgentUpdate | None:
    op = db.scalar(
        select(AgentUpdate)
        .where(AgentUpdate.device_id == device_id)
        .order_by(AgentUpdate.created_at.desc(), AgentUpdate.id.desc())
        .limit(1)
    )
    if op and op.state in UPDATE_ACTIVE:
        started = (
            op.restarting_at if op.state == "restarting" and op.restarting_at else op.created_at
        )
        if now() - started.replace(tzinfo=UTC) > timedelta(minutes=2):
            finish_update(db, op, "reconnect_timeout")
    return op


def serialize_device(device: Device, db: Session | None = None) -> dict:
    latest = cfg.jump_agent_version
    available = (
        newer_release(device.agent_version, latest)
        and device.os_family in ("linux", "windows")
        and device.architecture == "amd64"
    )
    op = latest_update(db, device.id) if db else None
    return {
        "id": str(device.id),
        "device_uuid": str(device.device_uuid),
        "hostname": device.hostname,
        "display_name": device.display_name,
        "os_family": device.os_family,
        "os_version": device.os_version,
        "architecture": device.architecture,
        "agent_version": device.agent_version,
        "agent_update": {
            "current_version": device.agent_version,
            "latest_version": latest if release_number(latest) else None,
            "update_available": available,
            "remote_update_supported": "agent_update_v1" in device.capabilities,
            "update_state": update_output(op) if op else None,
        },
        "capabilities": device.capabilities,
        "addresses": device.addresses,
        "primary_ip": device.primary_ip,
        "current_user": device.current_user,
        "group": {"id": str(device.group.id), "name": device.group.name} if device.group else None,
        "tags": [{"id": str(tag.id), "name": tag.name} for tag in device.tags],
        "online": device.online,
        "ssh_host_key": device.ssh_host_key,
        "identity_state": (
            "revoked"
            if device.agent_identity and device.agent_identity.revoked_at
            else "active"
            if device.agent_identity
            else "none"
        ),
        "last_seen_at": device.last_seen_at,
        "enrolled_at": device.enrolled_at,
    }


@app.get("/health")
def health():
    return {"status": "ok"}


@app.get("/auth/login")
async def login(request: Request):
    return await oauth.provider.authorize_redirect(request, cfg.oidc_redirect_uri)


@app.get("/auth/callback")
async def callback(request: Request, db: Session = Depends(get_db)):
    token = await oauth.provider.authorize_access_token(request)
    claims = token.get("userinfo")
    if not claims:
        raise HTTPException(401, "OIDC userinfo missing")
    user = map_oidc_user(db, cfg.oidc_issuer, claims)
    request.session.clear()
    request.session["uid"] = str(user.id)
    request.session["csrf"] = secrets.token_urlsafe(32)
    return RedirectResponse("/", status_code=303)


@app.post("/api/logout")
def logout(request: Request, user: User = Depends(current_user)):
    request.session.clear()
    return {"ok": True}


@app.get("/api/me")
def me(request: Request, user: User = Depends(current_user)):
    return {
        "id": user.id,
        "email": user.email,
        "display_name": user.display_name,
        "role": user.role,
        "csrf": request.session["csrf"],
    }


@app.get("/api/devices")
def devices(user: User = Depends(current_user), db: Session = Depends(get_db)):
    return [
        serialize_device(d, db) for d in db.scalars(select(Device).order_by(Device.hostname)).all()
    ]


@app.get("/api/devices/{device_id}")
def device(device_id: uuid.UUID, user: User = Depends(current_user), db: Session = Depends(get_db)):
    item = db.get(Device, device_id)
    if not item:
        raise HTTPException(404)
    return serialize_device(item, db)


@app.post("/api/devices/{device_id}/agent-update", status_code=201)
def request_agent_update(
    device_id: uuid.UUID,
    request: Request,
    user: User = Depends(admin),
    db: Session = Depends(get_db),
):
    if request.headers.get("content-length", "0") not in ("0", "2"):
        raise HTTPException(400, "Update target is selected by Jump")
    device = db.scalar(select(Device).where(Device.id == device_id).with_for_update())
    if not device:
        raise HTTPException(404)
    if not device.online or not device.connection_id:
        raise HTTPException(409, "Device is offline")
    if not device.agent_identity or device.agent_identity.revoked_at:
        raise HTTPException(409, "Agent identity is revoked")
    if device.os_family not in ("linux", "windows") or device.architecture != "amd64":
        raise HTTPException(409, "Unsupported agent platform")
    if "agent_update_v1" not in device.capabilities:
        raise HTTPException(409, "This agent must be updated manually once")
    if not newer_release(device.agent_version, cfg.jump_agent_version):
        raise HTTPException(409, "No newer supported agent release")
    if latest := latest_update(db, device.id):
        if latest.state in UPDATE_ACTIVE:
            raise HTTPException(409, "An agent update is already running")
    active = db.scalar(
        select(RemoteSession.id)
        .where(
            RemoteSession.device_id == device.id, RemoteSession.state.in_(["connecting", "active"])
        )
        .limit(1)
    )
    if active:
        raise HTTPException(409, "Close active sessions before updating this agent")
    try:
        asset = release_asset(cfg.jump_agent_version, device.os_family)
    except (ValueError, httpx.HTTPError) as exc:
        raise HTTPException(503, "Agent release metadata unavailable") from exc
    op = AgentUpdate(
        device_id=device.id,
        actor_user_id=user.id,
        from_version=device.agent_version,
        target_version=cfg.jump_agent_version,
        connection_id=device.connection_id,
    )
    db.add(op)
    db.flush()
    db.add(
        AuditEvent(
            event_type="agent_update_started",
            actor_user_id=user.id,
            device_id=device.id,
            request_id=request.state.request_id,
            detail={
                "operation_id": str(op.id),
                "from_version": op.from_version,
                "target_version": op.target_version,
            },
        )
    )
    db.commit()
    try:
        with httpx.Client(timeout=5) as client:
            response = client.post(
                f"{cfg.broker_internal_url.rstrip('/')}/internal/devices/{device.id}/agent-update",
                headers={"Authorization": f"Bearer {cfg.broker_internal_token}"},
                json={"operation_id": str(op.id), "connection_id": op.connection_id, **asset},
            )
            if response.status_code == 409:
                finish_update(db, op, "agent_unavailable")
                raise HTTPException(409, "Agent connection changed or sessions are active")
            response.raise_for_status()
    except httpx.HTTPError as exc:
        finish_update(db, op, "agent_unavailable")
        raise HTTPException(503, "Could not deliver agent update") from exc
    return update_output(op)


@app.patch("/api/devices/{device_id}")
def patch_device(
    device_id: uuid.UUID,
    patch: DevicePatch,
    request: Request,
    user: User = Depends(admin),
    db: Session = Depends(get_db),
):
    item = db.get(Device, device_id)
    if not item:
        raise HTTPException(404)
    if patch.group_id and not db.get(Group, patch.group_id):
        raise HTTPException(400, "Unknown group")
    tags = db.scalars(select(Tag).where(Tag.id.in_(patch.tag_ids))).all()
    if len(tags) != len(set(patch.tag_ids)):
        raise HTTPException(400, "Unknown tag")
    item.display_name, item.group_id, item.tags = patch.display_name, patch.group_id, tags
    db.add(
        AuditEvent(
            event_type="device_updated",
            actor_user_id=user.id,
            device_id=item.id,
            request_id=request.state.request_id,
        )
    )
    db.commit()
    return serialize_device(item)


def disconnect_revoked_agent(device_id: uuid.UUID) -> None:
    try:
        with httpx.Client(timeout=5) as client:
            response = client.post(
                f"{cfg.broker_internal_url.rstrip('/')}/internal/devices/{device_id}/disconnect",
                headers={"Authorization": f"Bearer {cfg.broker_internal_token}"},
            )
            response.raise_for_status()
    except httpx.HTTPError as exc:
        log.warning(
            json.dumps({"event": "revocation_disconnect_failed", "device_id": str(device_id)})
        )
        raise HTTPException(
            503, "Identity revoked, but the active broker disconnect could not be confirmed"
        ) from exc


@app.post("/api/devices/{device_id}/revoke")
def revoke_device(
    device_id: uuid.UUID,
    request: Request,
    user: User = Depends(admin),
    db: Session = Depends(get_db),
):
    # Lock in the same order as /connected, so a concurrent reconnect cannot
    # become active after the revocation transaction has committed.
    item = db.scalar(select(Device).where(Device.id == device_id).with_for_update())
    if not item:
        raise HTTPException(404)
    identity = db.scalar(
        select(AgentIdentity).where(AgentIdentity.device_id == device_id).with_for_update()
    )
    if not identity:
        raise HTTPException(404, "Device has no agent identity")
    if identity.revoked_at is None:
        identity.revoked_at = now()
        item.online = False
        item.connection_id = None
        db.add(
            AuditEvent(
                event_type="agent_identity_revoked",
                actor_user_id=user.id,
                device_id=item.id,
                request_id=request.state.request_id,
            )
        )
        db.commit()
    # Retry the broker disconnect on repeated requests if an earlier attempt
    # committed revocation but could not reach the broker.
    disconnect_revoked_agent(device_id)
    db.refresh(item)
    return serialize_device(item)


@app.delete("/api/devices/{device_id}")
def delete_device(
    device_id: uuid.UUID,
    request: Request,
    user: User = Depends(admin),
    db: Session = Depends(get_db),
):
    item = db.scalar(select(Device).where(Device.id == device_id).with_for_update())
    if not item:
        raise HTTPException(404)
    identity = db.scalar(
        select(AgentIdentity).where(AgentIdentity.device_id == device_id).with_for_update()
    )
    if not identity:
        raise HTTPException(409, "Device has no agent identity and cannot be deleted")
    if identity.revoked_at is None:
        raise HTTPException(409, "Revoke the agent identity before deleting this device")
    if item.online:
        raise HTTPException(409, "Device must be offline before deletion")

    former = {
        "device_id": str(item.id),
        "device_uuid": str(item.device_uuid),
        "hostname": item.hostname,
        "display_name": item.display_name,
    }
    db.add(
        AuditEvent(
            event_type="device_deleted",
            actor_user_id=user.id,
            device_id=None,
            detail=former,
            request_id=request.state.request_id,
        )
    )
    db.delete(item)
    db.commit()
    return {"ok": True}


@app.get("/api/groups")
def groups(user: User = Depends(current_user), db: Session = Depends(get_db)):
    return [{"id": g.id, "name": g.name} for g in db.scalars(select(Group).order_by(Group.name))]


@app.post("/api/groups")
def add_group(body: NameInput, user: User = Depends(admin), db: Session = Depends(get_db)):
    group = Group(name=body.name.strip())
    db.add(group)
    db.add(
        AuditEvent(event_type="group_created", actor_user_id=user.id, detail={"name": group.name})
    )
    db.commit()
    return {"id": group.id, "name": group.name}


@app.put("/api/groups/{group_id}")
def rename_group(
    group_id: uuid.UUID,
    body: NameInput,
    user: User = Depends(admin),
    db: Session = Depends(get_db),
):
    item = db.get(Group, group_id)
    if not item:
        raise HTTPException(404)
    item.name = body.name.strip()
    db.add(
        AuditEvent(event_type="group_updated", actor_user_id=user.id, detail={"id": str(group_id)})
    )
    db.commit()
    return {"id": item.id, "name": item.name}


@app.delete("/api/groups/{group_id}")
def delete_group(group_id: uuid.UUID, user: User = Depends(admin), db: Session = Depends(get_db)):
    item = db.get(Group, group_id)
    if not item:
        raise HTTPException(404)
    db.delete(item)
    db.add(
        AuditEvent(event_type="group_deleted", actor_user_id=user.id, detail={"id": str(group_id)})
    )
    db.commit()
    return {"ok": True}


@app.get("/api/tags")
def tags(user: User = Depends(current_user), db: Session = Depends(get_db)):
    return [{"id": t.id, "name": t.name} for t in db.scalars(select(Tag).order_by(Tag.name))]


@app.post("/api/tags")
def add_tag(body: NameInput, user: User = Depends(admin), db: Session = Depends(get_db)):
    tag = Tag(name=body.name.strip())
    db.add(tag)
    db.add(AuditEvent(event_type="tag_created", actor_user_id=user.id, detail={"name": tag.name}))
    db.commit()
    return {"id": tag.id, "name": tag.name}


@app.put("/api/tags/{tag_id}")
def rename_tag(
    tag_id: uuid.UUID,
    body: NameInput,
    user: User = Depends(admin),
    db: Session = Depends(get_db),
):
    item = db.get(Tag, tag_id)
    if not item:
        raise HTTPException(404)
    item.name = body.name.strip()
    db.add(AuditEvent(event_type="tag_updated", actor_user_id=user.id, detail={"id": str(tag_id)}))
    db.commit()
    return {"id": item.id, "name": item.name}


@app.delete("/api/tags/{tag_id}")
def delete_tag(tag_id: uuid.UUID, user: User = Depends(admin), db: Session = Depends(get_db)):
    item = db.get(Tag, tag_id)
    if not item:
        raise HTTPException(404)
    db.delete(item)
    db.add(AuditEvent(event_type="tag_deleted", actor_user_id=user.id, detail={"id": str(tag_id)}))
    db.commit()
    return {"ok": True}


@app.get("/api/agent-downloads")
def download_links(user: User = Depends(admin)):
    return agent_downloads(cfg.jump_server_version, cfg.jump_agent_version)


@app.post("/api/enrollment-tokens")
def new_enrollment(
    body: EnrollmentInput, user: User = Depends(admin), db: Session = Depends(get_db)
):
    if body.os_family not in ("windows", "linux"):
        raise HTTPException(400, "Select Windows or Linux")
    token, record = create_enrollment(db, user, body.os_family)
    return {
        "token": token,
        "id": record.id,
        "expires_at": record.expires_at,
        "agent_url": cfg.agent_url,
    }


@app.post("/api/enrollment-tokens/{token_id}/revoke")
def revoke(token_id: uuid.UUID, user: User = Depends(admin), db: Session = Depends(get_db)):
    record = db.get(EnrollmentToken, token_id)
    if not record:
        raise HTTPException(404)
    record.revoked_at = now()
    db.add(AuditEvent(event_type="enrollment_token_revoked", actor_user_id=user.id))
    db.commit()
    return {"ok": True}


@app.get("/api/audit")
def audit(user: User = Depends(admin), db: Session = Depends(get_db)):
    events = db.scalars(select(AuditEvent).order_by(AuditEvent.created_at.desc()).limit(100))
    return [
        {
            "id": e.id,
            "event_type": e.event_type,
            "actor_user_id": e.actor_user_id,
            "device_id": e.device_id,
            "detail": e.detail,
            "created_at": e.created_at,
        }
        for e in events
    ]


@app.get("/api/summary")
def summary(user: User = Depends(current_user), db: Session = Depends(get_db)):
    total = db.scalar(select(func.count(Device.id))) or 0
    online = db.scalar(select(func.count(Device.id)).where(Device.online.is_(True))) or 0
    return {"total": total, "online": online, "offline": total - online}


@app.post("/api/internal/enroll", dependencies=[Depends(internal)])
def enroll(body: EnrollRequest, db: Session = Depends(get_db)):
    try:
        public_key = base64.b64decode(body.public_key, validate=True)
    except ValueError as exc:
        raise HTTPException(400, "Invalid public key") from exc
    if len(public_key) != 32 or body.metadata.os_family not in ("linux", "windows"):
        raise HTTPException(400, "Invalid device identity or operating system")
    enrollment = consume_enrollment(db, body.token, body.metadata.os_family)
    device = Device(
        hostname=body.metadata.hostname,
        os_family=body.metadata.os_family,
        os_version=body.metadata.os_version,
        architecture=body.metadata.architecture,
        agent_version=body.metadata.agent_version,
        capabilities=body.metadata.capabilities,
        addresses=body.metadata.addresses,
        current_user=body.metadata.current_user,
        primary_ip=body.metadata.addresses[0] if body.metadata.addresses else None,
    )
    db.add(device)
    db.flush()
    db.add(AgentIdentity(device_id=device.id, public_key=public_key))
    db.add(
        AuditEvent(
            event_type="device_enrolled",
            actor_user_id=enrollment.created_by_id,
            device_id=device.id,
        )
    )
    db.commit()
    return {"device_id": str(device.id), "device_uuid": str(device.device_uuid)}


@app.get("/api/internal/identities/{device_id}", dependencies=[Depends(internal)])
def identity(device_id: uuid.UUID, db: Session = Depends(get_db)):
    identity = db.scalar(
        select(AgentIdentity).where(
            AgentIdentity.device_id == device_id, AgentIdentity.revoked_at.is_(None)
        )
    )
    if not identity:
        raise HTTPException(404)
    return {"public_key": base64.b64encode(identity.public_key).decode()}


def apply_metadata(device: Device, data: Metadata) -> None:
    if data.os_family != device.os_family:
        raise HTTPException(400, "OS family cannot change")
    for attr in (
        "hostname",
        "os_version",
        "architecture",
        "agent_version",
        "capabilities",
        "addresses",
        "current_user",
    ):
        setattr(device, attr, getattr(data, attr))
    device.primary_ip = data.addresses[0] if data.addresses else None


@app.post("/api/internal/devices/{device_id}/connected", dependencies=[Depends(internal)])
def connected(device_id: uuid.UUID, body: PresenceInput, db: Session = Depends(get_db)):
    device = db.scalar(select(Device).where(Device.id == device_id).with_for_update())
    if not device:
        raise HTTPException(404)
    identity = db.scalar(
        select(AgentIdentity)
        .where(AgentIdentity.device_id == device_id, AgentIdentity.revoked_at.is_(None))
        .with_for_update()
    )
    if not identity:
        raise HTTPException(403, "Agent identity revoked")
    operation = latest_update(db, device_id)
    if body.metadata:
        apply_metadata(device, body.metadata)
    device.online, device.last_seen_at, device.connection_id = True, now(), body.connection_id
    db.add(AuditEvent(event_type="agent_connected", device_id=device.id))
    db.commit()
    if (
        operation
        and operation.state in UPDATE_ACTIVE
        and body.connection_id != operation.connection_id
    ):
        if body.metadata and body.metadata.agent_version == operation.target_version:
            finish_update(db, operation)
        elif (
            body.metadata
            and body.metadata.agent_version == operation.from_version
            and operation.state == "restarting"
        ):
            finish_update(db, operation, "rollback_completed")
    return {"ok": True}


@app.post("/api/internal/devices/{device_id}/agent-update-status", dependencies=[Depends(internal)])
def agent_update_status(device_id: uuid.UUID, body: dict, db: Session = Depends(get_db)):
    if set(body) != {"operation_id", "connection_id", "state", "reason"}:
        raise HTTPException(400, "Invalid update status")
    try:
        op = db.get(AgentUpdate, uuid.UUID(body["operation_id"]))
    except (ValueError, TypeError, AttributeError):
        raise HTTPException(400, "Invalid operation") from None
    device = db.get(Device, device_id)
    if (
        not op
        or not device
        or op.device_id != device_id
        or op.connection_id != body["connection_id"]
        or device.connection_id != body["connection_id"]
        or op.state not in UPDATE_ACTIVE
    ):
        raise HTTPException(409, "Update connection changed")
    state = body["state"]
    if state == "failed":
        if body["reason"] not in UPDATE_REASONS:
            raise HTTPException(400, "Invalid failure reason")
        finish_update(db, op, body["reason"])
    elif state in ("downloading", "installing", "restarting") and body["reason"] is None:
        if ("pending", "downloading", "installing", "restarting").index(
            state
        ) < UPDATE_ACTIVE.index(op.state):
            raise HTTPException(409, "Invalid update transition")
        op.state = state
        if not op.started_at:
            op.started_at = now()
        if state == "restarting" and not op.restarting_at:
            op.restarting_at = now()
        db.commit()
    else:
        raise HTTPException(400, "Invalid update status")
    return {"ok": True}


@app.post("/api/internal/devices/{device_id}/heartbeat", dependencies=[Depends(internal)])
def heartbeat(device_id: uuid.UUID, body: PresenceInput, db: Session = Depends(get_db)):
    result = db.execute(
        update(Device)
        .where(
            Device.id == device_id,
            Device.connection_id == body.connection_id,
            Device.online.is_(True),
            Device.id.in_(
                select(AgentIdentity.device_id).where(AgentIdentity.revoked_at.is_(None))
            ),
        )
        .values(last_seen_at=now())
    )
    db.commit()
    if not result.rowcount:
        raise HTTPException(409, "Connection replaced")
    return {"ok": True}


@app.post("/api/internal/devices/{device_id}/disconnected", dependencies=[Depends(internal)])
def disconnected(device_id: uuid.UUID, body: PresenceInput, db: Session = Depends(get_db)):
    result = db.execute(
        update(Device)
        .where(Device.id == device_id, Device.connection_id == body.connection_id)
        .values(online=False, connection_id=None)
    )
    if result.rowcount:
        db.add(AuditEvent(event_type="agent_disconnected", device_id=device_id))
    db.commit()
    return {"ok": True}


@app.post("/api/internal/reconcile", dependencies=[Depends(internal)])
def reconcile(db: Session = Depends(get_db)):
    # Called at broker startup; a process restart invalidates its in-memory connections.
    db.execute(
        update(Device).where(Device.online.is_(True)).values(online=False, connection_id=None)
    )
    for session in db.scalars(
        select(RemoteSession).where(RemoteSession.state.in_(["connecting", "active"]))
    ).all():
        finish_ssh_session(db, session, "service_restarted")
    db.commit()
    return {"ok": True}


SSH_KINDS = {"linux_password", "linux_ssh_key"}
SSH_ERRORS = {
    "authentication_failed": "SSH authentication failed",
    "ssh_unavailable": "SSH service unavailable on this device",
    "timeout": "SSH connection timed out",
    "host_key_mismatch": "SSH host key changed. An admin must verify and reset it.",
    "device_disconnected": "Device disconnected",
    "agent_unavailable": "Agent unavailable",
    "unsupported_agent": "Update the Jump agent to enable browser SSH",
    "unsupported_credential": "Unsupported SSH credential",
    "session_expired": "Session expired",
    "idle_timeout": "Session closed after 30 minutes of inactivity",
    "session_closed": "SSH session ended",
}


def credential_output(item: Credential) -> dict:
    return {"id": item.id, "label": item.label, "kind": item.kind, "username": item.username}


@app.get("/api/devices/{device_id}/credentials")
def list_credentials(
    device_id: uuid.UUID, user: User = Depends(admin), db: Session = Depends(get_db)
):
    if not db.get(Device, device_id):
        raise HTTPException(404)
    return [
        credential_output(c)
        for c in db.scalars(select(Credential).where(Credential.device_id == device_id)).all()
    ]


@app.post("/api/devices/{device_id}/credentials", status_code=201)
def add_credential(
    device_id: uuid.UUID,
    body: CredentialInput,
    user: User = Depends(admin),
    db: Session = Depends(get_db),
):
    device = db.get(Device, device_id)
    if not device:
        raise HTTPException(404)
    if device.os_family != "linux":
        raise HTTPException(400, "SSH credentials require a Linux device")
    secret_bytes = body.secret.encode()
    if len(secret_bytes) > 16384:
        raise HTTPException(400, "Credential is too large")
    try:
        item = create_credential(
            db,
            user,
            device,
            label=body.label,
            kind=body.kind,
            username=body.username,
            secret=secret_bytes,
        )
    except ValueError as exc:
        raise HTTPException(400, "Invalid credential") from exc
    return credential_output(item)


@app.post("/api/devices/{device_id}/ssh-host-key/reset")
def reset_ssh_host_key(
    device_id: uuid.UUID,
    request: Request,
    user: User = Depends(admin),
    db: Session = Depends(get_db),
):
    device = db.scalar(select(Device).where(Device.id == device_id).with_for_update())
    if not device:
        raise HTTPException(404)
    if db.scalar(
        select(RemoteSession.id).where(
            RemoteSession.device_id == device_id,
            (RemoteSession.state == "active")
            | (
                (RemoteSession.state == "connecting")
                & (RemoteSession.created_at > now() - timedelta(seconds=60))
            ),
        )
    ):
        raise HTTPException(409, "Close active SSH sessions before resetting the host key")
    device.ssh_host_key = None
    db.add(
        AuditEvent(
            event_type="ssh_host_key_reset",
            actor_user_id=user.id,
            device_id=device_id,
            request_id=request.state.request_id,
        )
    )
    db.commit()
    return {"ok": True}


@app.post("/api/devices/{device_id}/ssh-sessions", status_code=201)
def new_ssh_session(
    device_id: uuid.UUID,
    body: SSHSessionInput,
    request: Request,
    user: User = Depends(admin),
    db: Session = Depends(get_db),
):
    device = db.get(Device, device_id)
    if not device:
        raise HTTPException(404)
    if not device.online or not device.connection_id:
        raise HTTPException(409, "Device is offline")
    if not device.agent_identity or device.agent_identity.revoked_at:
        raise HTTPException(409, "Agent identity is revoked")
    if device.os_family != "linux":
        raise HTTPException(409, "Device does not support SSH")
    if "ssh_terminal_v1" not in device.capabilities:
        raise HTTPException(409, "Update the Jump agent to enable browser SSH")
    credential = db.get(Credential, body.credential_id)
    if not credential or credential.device_id != device_id:
        raise HTTPException(400, "Credential does not belong to this device")
    if credential.kind not in SSH_KINDS:
        raise HTTPException(400, "Unsupported SSH credential")
    session = RemoteSession(
        id=uuid.uuid4(),
        device_id=device_id,
        user_id=user.id,
        credential_id=credential.id,
        columns=body.columns,
        rows=body.rows,
    )
    db.add(session)
    db.commit()
    return {"id": session.id, "state": session.state, "created_at": session.created_at}


def finish_ssh_session(db: Session, session: RemoteSession, reason: str | None = None) -> None:
    if session.state in ("closed", "failed"):
        return
    was_active = session.state == "active"
    session.state = (
        "closed" if was_active and reason in (None, "session_closed", "idle_timeout") else "failed"
    )
    session.closed_at = now()
    session.failure_reason = reason if session.state == "failed" else None
    db.add(
        AuditEvent(
            event_type="ssh_session_ended" if session.state == "closed" else "ssh_session_failed",
            actor_user_id=session.user_id,
            device_id=session.device_id,
            detail={
                "session_id": str(session.id),
                "credential_id": str(session.credential_id),
                "reason": reason or "disconnected",
            },
        )
    )
    db.commit()


def trust_ssh_host_key(db: Session, session: RemoteSession, fingerprint: str) -> bool:
    if not isinstance(fingerprint, str) or not re.fullmatch(
        r"SHA256:[A-Za-z0-9+/]{43}", fingerprint
    ):
        return False
    device = db.scalar(select(Device).where(Device.id == session.device_id).with_for_update())
    if (
        not device
        or not device.online
        or not device.agent_identity
        or device.agent_identity.revoked_at
    ):
        return False
    if device.ssh_host_key and device.ssh_host_key != fingerprint:
        return False
    if not device.ssh_host_key:
        device.ssh_host_key = fingerprint
        db.add(
            AuditEvent(
                event_type="ssh_host_key_trusted",
                actor_user_id=session.user_id,
                device_id=device.id,
                detail={"fingerprint": fingerprint, "session_id": str(session.id)},
            )
        )
    db.commit()
    return True


@app.websocket("/ws/sessions/{session_id}")
async def browser_ssh_session(ws: WebSocket, session_id: uuid.UUID, db: Session = Depends(get_db)):
    # WebSocket upgrades bypass the HTTP CSRF middleware. Same-origin and the
    # signed browser session are mandatory; the cookie alone is insufficient.
    origin = ws.headers.get("origin")
    if origin != cfg.public_url.rstrip("/"):
        await ws.close(code=1008)
        return
    try:
        uid = uuid.UUID(ws.session.get("uid", ""))
    except ValueError:
        await ws.close(code=1008)
        return
    session = db.get(RemoteSession, session_id)
    user = db.get(User, uid)
    if not user or not session or session.user_id != user.id or session.state != "connecting":
        await ws.close(code=1008)
        return
    created_at = (
        session.created_at.replace(tzinfo=UTC)
        if session.created_at.tzinfo is None
        else session.created_at
    )
    if created_at < now() - timedelta(seconds=60):
        finish_ssh_session(db, session, "session_expired")
        await ws.close(code=1008)
        return
    # Atomic claim blocks duplicate attaches across workers and processes.
    claimed = db.execute(
        update(RemoteSession)
        .where(
            RemoteSession.id == session_id,
            RemoteSession.attached_at.is_(None),
            RemoteSession.state == "connecting",
        )
        .values(attached_at=now())
    )
    db.commit()
    if not claimed.rowcount:
        await ws.close(code=1008)
        return
    await ws.accept()
    reason = "agent_unavailable"
    backend = None
    try:
        device = db.get(Device, session.device_id)
        credential = db.get(Credential, session.credential_id)
        if (
            not device
            or not device.online
            or not device.agent_identity
            or device.agent_identity.revoked_at
            or not credential
        ):
            raise RuntimeError("device_disconnected")
        if "ssh_terminal_v1" not in device.capabilities:
            raise RuntimeError("unsupported_agent")
        secret = bytearray(decrypt_for_gateway(credential))
        try:
            address = (
                cfg.broker_internal_url.rstrip("/")
                .replace("http://", "ws://", 1)
                .replace("https://", "wss://", 1)
            )
            backend = await ws_connect(
                f"{address}/internal/sessions/{session.id}?device_id={device.id}",
                additional_headers={"Authorization": "Bearer " + cfg.broker_internal_token},
                max_size=65536,
                open_timeout=10,
                close_timeout=2,
            )
            await backend.send(
                json.dumps(
                    {
                        "version": 1,
                        "type": "session_open",
                        "session_id": str(session.id),
                        "kind": credential.kind,
                        "username": credential.username,
                        "secret": base64.b64encode(secret).decode(),
                        "host_key": device.ssh_host_key or "",
                        "columns": session.columns,
                        "rows": session.rows,
                    }
                )
            )
        finally:
            secret[:] = b"\0" * len(secret)
            del secret

        first = json.loads(await asyncio.wait_for(backend.recv(), timeout=15))
        if first.get("session_id") != str(session.id):
            raise RuntimeError("agent_unavailable")
        if first.get("type") == "session_error":
            raise RuntimeError(
                first.get("code") if first.get("code") in SSH_ERRORS else "agent_unavailable"
            )
        if first.get("type") != "session_opened" or not trust_ssh_host_key(
            db, session, first.get("fingerprint", "")
        ):
            raise RuntimeError("host_key_mismatch")
        session.state, session.connected_at, session.last_activity_at = "active", now(), now()
        db.add(
            AuditEvent(
                event_type="ssh_session_started",
                actor_user_id=user.id,
                device_id=device.id,
                detail={"session_id": str(session.id), "credential_id": str(credential.id)},
            )
        )
        db.commit()
        await ws.send_json(
            {"type": "status", "state": "active", "fingerprint": device.ssh_host_key}
        )
        last_activity = time.monotonic()

        async def browser_to_agent():
            nonlocal last_activity
            while True:
                raw = await ws.receive_text()
                if len(raw) > 16384:
                    raise RuntimeError("session_closed")
                frame = json.loads(raw)
                if frame.get("type") == "session_data":
                    data = base64.b64decode(frame.get("data", ""), validate=True)
                    if len(data) > 8192:
                        raise RuntimeError("session_closed")
                    if data:
                        last_activity = time.monotonic()
                        session.last_activity_at = now()
                        db.commit()
                elif frame.get("type") == "session_resize":
                    if not (
                        20 <= frame.get("columns", 0) <= 500 and 5 <= frame.get("rows", 0) <= 200
                    ):
                        raise RuntimeError("session_closed")
                elif frame.get("type") == "session_close":
                    return "session_closed"
                else:
                    raise RuntimeError("session_closed")
                frame.update(version=1, session_id=str(session.id))
                await backend.send(json.dumps(frame))

        async def agent_to_browser():
            nonlocal last_activity
            while True:
                try:
                    raw = await asyncio.wait_for(backend.recv(), timeout=5)
                except TimeoutError:
                    if time.monotonic() - last_activity >= cfg.ssh_idle_seconds:
                        return "idle_timeout"
                    continue
                if len(raw) > 65536:
                    return "session_closed"
                frame = json.loads(raw)
                if frame.get("session_id") != str(session.id):
                    return "session_closed"
                if frame.get("type") == "session_data":
                    if len(base64.b64decode(frame.get("data", ""), validate=True)) > 8192:
                        return "session_closed"
                    last_activity = time.monotonic()
                    session.last_activity_at = now()
                    db.commit()
                    await ws.send_json({"type": "session_data", "data": frame["data"]})
                elif frame.get("type") == "session_close":
                    return "session_closed"
                elif frame.get("type") == "session_error":
                    return (
                        frame.get("code")
                        if frame.get("code") in SSH_ERRORS
                        else "agent_unavailable"
                    )
                else:
                    return "session_closed"

        tasks = [asyncio.create_task(browser_to_agent()), asyncio.create_task(agent_to_browser())]
        try:
            done, _ = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
            reason = next(iter(done)).result()
        finally:
            for task in tasks:
                if not task.done():
                    task.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)
        if reason == "idle_timeout":
            db.add(
                AuditEvent(
                    event_type="ssh_session_idle_timeout",
                    actor_user_id=user.id,
                    device_id=device.id,
                    detail={"session_id": str(session.id)},
                )
            )
            db.commit()
    except WebSocketDisconnect:
        reason = "session_closed"
    except ConnectionClosed:
        reason = "device_disconnected"
    except (TimeoutError, OSError):
        reason = "timeout" if session.state == "connecting" else "agent_unavailable"
    except (
        RuntimeError,
        ValueError,
        TypeError,
        AttributeError,
        KeyError,
        UnicodeDecodeError,
        json.JSONDecodeError,
        binascii.Error,
    ) as exc:
        reason = str(exc) if str(exc) in SSH_ERRORS else "agent_unavailable"
    finally:
        if backend:
            try:
                await backend.send(
                    json.dumps(
                        {"version": 1, "type": "session_close", "session_id": str(session.id)}
                    )
                )
                await backend.close()
            except (ConnectionClosed, OSError):
                pass
        finish_ssh_session(db, session, reason)
        try:
            await ws.send_json(
                {
                    "type": "status",
                    "state": "closed",
                    "code": reason,
                    "message": SSH_ERRORS.get(reason, "Session disconnected"),
                }
            )
            await ws.close()
        except (RuntimeError, WebSocketDisconnect, OSError):
            pass


static_root = Path("/opt/jump/static")
if (static_root / "assets").exists():
    app.mount("/assets", StaticFiles(directory=static_root / "assets"), name="assets")


@app.get("/{path:path}")
def frontend(path: str):
    if path.startswith(("api/", "auth/", "assets/")):
        raise HTTPException(404)
    if not (static_root / "index.html").exists():
        raise HTTPException(404)
    return FileResponse(static_root / "index.html")
