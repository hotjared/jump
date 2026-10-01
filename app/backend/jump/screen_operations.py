"""Transient explicit Screen operations; clipboard text never enters persistence."""

import base64
import time
import uuid

CLIPBOARD_MAX = 1024 * 1024
CLIPBOARD_CHUNK = 16384
CLIPBOARD_WIRE_MAX = 24576
OPERATION_CODES = {
    "ok": "",
    "unsupported_agent": "Update the Windows Jump agent to enable unattended admin controls.",
    "operation_cancelled": "Screen action cancelled.",
    "control_required": "Switch to Control to use this action.",
    "operation_busy": "A Screen action is already in progress.",
    "operation_timeout": "Screen action timed out.",
    "sas_blocked": "Windows policy blocked remote Ctrl+Alt+Del.",
    "sas_unavailable": "Windows could not send remote Ctrl+Alt+Del.",
    "clipboard_unavailable": "The Windows clipboard is unavailable.",
    "clipboard_too_large": "Clipboard text is too large (1 MiB maximum).",
    "invalid_clipboard": "Invalid Screen clipboard transfer.",
}
OPERATION_FIELDS = {
    "screen_operation": {"type", "request_id", "kind"},
    "screen_clipboard": {"type", "request_id", "index", "count", "data"},
    "screen_clipboard_ack": {"type", "request_id", "index"},
    "screen_operation_cancel": {"type", "request_id"},
}
DESKTOPS = {"Default", "Winlogon", "ScreenSaver"}
EVENTS = {"desktop_attached", "desktop_changed", "sas_requested", "sas_sent"}


class ScreenOperation:
    def __init__(self):
        self.clear()

    def clear(self):
        if getattr(self, "id", ""):
            self.last_id, self.last_kind, self.last_index = self.id, self.kind, self.index
        else:
            self.last_id = getattr(self, "last_id", "")
            self.last_kind = getattr(self, "last_kind", "")
            self.last_index = getattr(self, "last_index", 0)
        self.id = self.kind = ""
        self.started = 0.0
        self.index = self.count = 0
        self.waiting = False
        self.cancelled = False
        self.data = bytearray()

    def request(self, frame):
        kind = frame["type"]
        if kind == "screen_operation":
            rid = frame.get("request_id")
            try:
                valid_id = isinstance(rid, str) and str(uuid.UUID(rid)) == rid
            except (ValueError, TypeError):
                valid_id = False
            if (
                self.id
                or not valid_id
                or frame.get("kind") not in ("sas", "clipboard_set", "clipboard_get")
            ):
                raise ValueError("invalid_frame")
            self.id, self.kind, self.started = rid, frame["kind"], time.monotonic()
            return
        if not self.id and self.last_id and frame.get("request_id") == self.last_id:
            if kind == "screen_operation_cancel" or (
                kind == "screen_clipboard_ack"
                and self.last_kind == "clipboard_get"
                and type(frame.get("index", 0)) is int
                and frame.get("index", 0) == self.last_index - 1
            ):
                return
        self.match(frame)
        if kind == "screen_operation_cancel":
            self.cancelled = True
            return
        elif kind == "screen_clipboard" and self.kind == "clipboard_set":
            self.chunk(frame)
        elif kind == "screen_clipboard_ack" and self.kind == "clipboard_get":
            self.ack(frame)
        else:
            raise ValueError("invalid_frame")

    def response(self, frame):
        self.match(frame)
        kind = frame["type"]
        if self.cancelled and kind != "screen_operation_result":
            return
        if kind == "screen_clipboard" and self.kind == "clipboard_get":
            self.chunk(frame)
        elif kind == "screen_clipboard_ack" and self.kind == "clipboard_set":
            self.ack(frame)
        elif kind == "screen_operation_result":
            code = frame.get("code")
            if frame.get("kind") != self.kind or code not in OPERATION_CODES or frame.get("data"):
                raise ValueError("invalid_frame")
            if (
                not self.cancelled
                and code == "ok"
                and self.kind != "sas"
                and (not self.count or self.index != self.count or self.waiting)
            ):
                raise ValueError("invalid_frame")
            self.clear()
        else:
            raise ValueError("invalid_frame")

    def match(self, frame):
        if not self.id or frame.get("request_id") != self.id:
            raise ValueError("invalid_frame")

    def chunk(self, frame):
        index, count, data = frame.get("index", 0), frame.get("count", 0), frame.get("data", "")
        if (
            type(index) is not int
            or type(count) is not int
            or self.waiting
            or index != self.index
            or not 1 <= count <= 64
            or index >= count
            or self.count
            and count != self.count
            or not isinstance(data, str)
            or len(data) > 21848
        ):
            raise ValueError("invalid_frame")
        chunk = base64.b64decode(data, validate=True)
        if (
            len(chunk) > CLIPBOARD_CHUNK
            or index < count - 1
            and len(chunk) != CLIPBOARD_CHUNK
            or not chunk
            and count != 1
            or len(self.data) + len(chunk) > CLIPBOARD_MAX
        ):
            raise ValueError("invalid_frame")
        self.data.extend(chunk)
        self.index += 1
        self.count, self.waiting = count, True
        if self.index == count:
            try:
                if "\0" in self.data.decode("utf-8"):
                    raise ValueError("invalid_frame")
            except UnicodeDecodeError:
                raise ValueError("invalid_frame") from None

    def ack(self, frame):
        index = frame.get("index", 0)
        if (
            type(index) is not int
            or not self.waiting
            or index != self.index - 1
            or frame.get("data")
        ):
            raise ValueError("invalid_frame")
        self.waiting = False
