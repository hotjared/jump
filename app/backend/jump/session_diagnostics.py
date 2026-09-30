"""Small, fixed vocabulary of observable remote session transitions."""

import logging
import uuid

from sqlalchemy import select
from sqlalchemy.exc import SQLAlchemyError
from sqlalchemy.orm import Session

from .models import RemoteSessionDiagnosticEvent

log = logging.getLogger(__name__)

STAGES = frozenset(
    {
        "session_created",
        "browser_attached",
        "broker_connected",
        "agent_stream_opened",
        "agent_tunnel_opened",
        "host_key_verified",
        "guacd_connected",
        "guacd_handshake_started",
        "protocol_ready",
        "session_active",
        "agent_session_opened",
        "interactive_session_found",
        "capture_started",
        "session_closed",
        "session_failed",
    }
)


def record_stage(db: Session, session_id: uuid.UUID, stage: str) -> None:
    if stage not in STAGES:
        raise ValueError("Unknown session diagnostic stage")
    try:
        with db.begin_nested():
            present = db.scalar(
                select(RemoteSessionDiagnosticEvent.id)
                .where(
                    RemoteSessionDiagnosticEvent.session_id == session_id,
                    RemoteSessionDiagnosticEvent.stage == stage,
                )
                .limit(1)
            )
            if not present:
                db.add(RemoteSessionDiagnosticEvent(session_id=session_id, stage=stage))
    except SQLAlchemyError:
        log.warning("Could not record remote session diagnostic stage")
