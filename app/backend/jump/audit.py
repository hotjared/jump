"""Presentation-safe audit detail. Never serialize the stored JSON directly."""

import re

FIELDS = {
    **{
        f"{protocol}_session_{state}": ("session_id", "reason")
        for protocol in ("ssh", "rdp")
        for state in ("started", "ended", "failed")
    },
    "ssh_session_idle_timeout": ("session_id",),
    "ssh_host_key_trusted": ("session_id", "fingerprint"),
    **{
        f"file_{direction}_{state}": ("transfer_id", "filename", "size", "reason")
        for direction in ("upload", "download")
        for state in ("started", "completed", "failed", "cancelled")
    },
    **{
        f"agent_update_{state}": ("operation_id", "from_version", "target_version", "reason")
        for state in ("started", "completed", "failed")
    },
    **{f"credential_{state}": ("label", "kind") for state in ("created", "updated", "deleted")},
    "group_created": ("name",),
    "tag_created": ("name",),
}
IDENTIFIER = re.compile(r"[a-z][a-z0-9_]{0,79}\Z")
UUID = re.compile(r"[a-fA-F0-9]{8}-[a-fA-F0-9-]{0,28}[a-fA-F0-9]\Z")
FINGERPRINT = re.compile(r"SHA256:[A-Za-z0-9+/=]{20,64}\Z")


def safe_detail(event_type: str, detail: dict | None) -> dict:
    if not isinstance(detail, dict):
        return {}
    result = {}
    for key in FIELDS.get(event_type, ()):
        value = detail.get(key)
        if key == "size":
            if type(value) is int and 0 <= value <= 10**15:
                result[key] = value
        elif isinstance(value, str):
            if key == "reason" and IDENTIFIER.fullmatch(value):
                result[key] = value
            elif key in ("session_id", "transfer_id", "operation_id") and UUID.fullmatch(value):
                result[key] = value
            elif key == "fingerprint" and FINGERPRINT.fullmatch(value):
                result[key] = value
            elif key in ("filename", "label", "name", "kind", "from_version", "target_version"):
                if 0 < len(value) <= 180 and not re.search(r"[\\/\x00-\x1f\x7f]", value):
                    result[key] = value
    return result
