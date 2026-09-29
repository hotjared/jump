import uuid

from sqlalchemy.orm import Session

from .models import AuditEvent, Credential, User
from .security import decrypt_secret, encrypt_secret, master_key

KINDS = {
    "windows_password",
    "linux_password",
    "linux_ssh_key",
}


def create_credential(
    db: Session,
    actor: User,
    *,
    label: str,
    kind: str,
    username: str,
    secret: bytes,
    domain: str | None = None,
) -> Credential:
    if kind not in KINDS or not label or not username or not secret:
        raise ValueError("Invalid credential metadata")
    if domain and kind != "windows_password":
        raise ValueError("Domain is only supported for Windows credentials")
    item = Credential(
        id=uuid.uuid4(),
        user_id=actor.id,
        label=label,
        kind=kind,
        username=username,
        domain=domain,
        ciphertext=b"",
        nonce=b"",
        key_version=1,
    )
    item.ciphertext, item.nonce, item.key_version = encrypt_secret(
        secret, str(item.id), master_key()
    )
    db.add(item)
    db.add(
        AuditEvent(
            event_type="credential_created",
            actor_user_id=actor.id,
            detail={"credential_id": str(item.id), "kind": item.kind, "label": item.label},
        )
    )
    db.commit()
    return item


def decrypt_for_gateway(item: Credential) -> bytes:
    """Server-side use only; never serialize the result into an API response."""
    if item.key_version != 1:
        raise ValueError("Unknown credential key version")
    return decrypt_secret(item.ciphertext, item.nonce, str(item.id), master_key())
