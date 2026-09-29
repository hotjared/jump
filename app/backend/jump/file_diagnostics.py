"""Fixed, observable file transfer lifecycle stages."""

import logging
import uuid

from sqlalchemy import select
from sqlalchemy.exc import SQLAlchemyError
from sqlalchemy.orm import Session

from .models import FileTransferDiagnosticEvent

log = logging.getLogger(__name__)

STAGES = frozenset(
    {
        "transfer_created",
        "broker_connected",
        "agent_opened",
        "streaming_started",
        "final_chunk_acknowledged",
        "agent_finished",
        "checksum_verified",
        "transfer_completed",
        "transfer_failed",
        "transfer_cancelled",
    }
)


def record_file_stage(db: Session, transfer_id: uuid.UUID, stage: str) -> None:
    if stage not in STAGES:
        raise ValueError("Unknown file transfer diagnostic stage")
    try:
        with db.begin_nested():
            present = db.scalar(
                select(FileTransferDiagnosticEvent.id)
                .where(
                    FileTransferDiagnosticEvent.transfer_id == transfer_id,
                    FileTransferDiagnosticEvent.stage == stage,
                )
                .limit(1)
            )
            if not present:
                db.add(FileTransferDiagnosticEvent(transfer_id=transfer_id, stage=stage))
    except SQLAlchemyError:
        log.warning("Could not record file transfer diagnostic stage")
