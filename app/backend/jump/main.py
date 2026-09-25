import base64
import hmac
import json
import logging
import secrets
import time
import uuid
from pathlib import Path

from authlib.integrations.starlette_client import OAuth
from fastapi import Depends, FastAPI, Header, HTTPException, Request, Response
from fastapi.responses import FileResponse, RedirectResponse
from fastapi.staticfiles import StaticFiles
from sqlalchemy import func, select, update
from sqlalchemy.orm import Session
from starlette.middleware.sessions import SessionMiddleware
from starlette.middleware.trustedhost import TrustedHostMiddleware

from .config import settings
from .db import get_db
from .models import AgentIdentity, AuditEvent, Device, EnrollmentToken, Group, Tag, User, now
from .schemas import DevicePatch, EnrollmentInput, EnrollRequest, Metadata, NameInput, PresenceInput
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


def serialize_device(device: Device) -> dict:
    return {
        "id": str(device.id),
        "device_uuid": str(device.device_uuid),
        "hostname": device.hostname,
        "display_name": device.display_name,
        "os_family": device.os_family,
        "os_version": device.os_version,
        "architecture": device.architecture,
        "agent_version": device.agent_version,
        "capabilities": device.capabilities,
        "addresses": device.addresses,
        "primary_ip": device.primary_ip,
        "current_user": device.current_user,
        "group": {"id": str(device.group.id), "name": device.group.name} if device.group else None,
        "tags": [{"id": str(tag.id), "name": tag.name} for tag in device.tags],
        "online": device.online,
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
    return [serialize_device(d) for d in db.scalars(select(Device).order_by(Device.hostname)).all()]


@app.get("/api/devices/{device_id}")
def device(device_id: uuid.UUID, user: User = Depends(current_user), db: Session = Depends(get_db)):
    item = db.get(Device, device_id)
    if not item:
        raise HTTPException(404)
    return serialize_device(item)


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
    device = db.get(Device, device_id)
    if not device:
        raise HTTPException(404)
    if body.metadata:
        apply_metadata(device, body.metadata)
    device.online, device.last_seen_at, device.connection_id = True, now(), body.connection_id
    db.add(AuditEvent(event_type="agent_connected", device_id=device.id))
    db.commit()
    return {"ok": True}


@app.post("/api/internal/devices/{device_id}/heartbeat", dependencies=[Depends(internal)])
def heartbeat(device_id: uuid.UUID, body: PresenceInput, db: Session = Depends(get_db)):
    result = db.execute(
        update(Device)
        .where(
            Device.id == device_id,
            Device.connection_id == body.connection_id,
            Device.online.is_(True),
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
    db.commit()
    return {"ok": True}


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
