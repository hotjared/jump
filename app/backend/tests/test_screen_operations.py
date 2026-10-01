import base64
import uuid

import pytest

from jump.screen_operations import CLIPBOARD_CHUNK, CLIPBOARD_MAX, ScreenOperation

RID = str(uuid.uuid4())


def request(op, kind):
    op.request({"type": "screen_operation", "request_id": RID, "kind": kind})


@pytest.mark.parametrize(
    "text", ["", "héllo 世界 😀", "x" * CLIPBOARD_MAX, "x" * (CLIPBOARD_CHUNK - 1) + "😀"]
)
@pytest.mark.parametrize("kind", ["clipboard_set", "clipboard_get"])
def test_clipboard_unicode_empty_boundary_credit_and_cleanup(text, kind):
    op = ScreenOperation()
    request(op, kind)
    encoded = text.encode()
    count = max(1, (len(encoded) + CLIPBOARD_CHUNK - 1) // CLIPBOARD_CHUNK)
    for index in range(count):
        chunk = {
            "type": "screen_clipboard",
            "request_id": RID,
            "index": index,
            "count": count,
            "data": base64.b64encode(
                encoded[index * CLIPBOARD_CHUNK : (index + 1) * CLIPBOARD_CHUNK]
            ).decode(),
        }
        ack = {"type": "screen_clipboard_ack", "request_id": RID, "index": index}
        incoming, outgoing = (
            (op.request, op.response) if kind == "clipboard_set" else (op.response, op.request)
        )
        incoming(chunk)
        with pytest.raises(ValueError):
            incoming(chunk)  # Duplicate and next chunk before credit are forbidden.
        outgoing(ack)
    assert op.data.decode() == text
    op.response({"type": "screen_operation_result", "request_id": RID, "kind": kind, "code": "ok"})
    assert not op.id and not op.data


@pytest.mark.parametrize(
    "change",
    [
        {"index": 1},
        {"count": 65},
        {"count": True},
        {"index": True},
        {"data": "!"},
        {"data": base64.b64encode(b"x" * (CLIPBOARD_CHUNK + 1)).decode()},
        {"data": base64.b64encode(b"\xff").decode()},
        {"data": base64.b64encode(b"\0").decode()},
        {"request_id": "wrong"},
        {"count": 2},
    ],
)
def test_malformed_oversized_missing_and_out_of_order_clipboard(change):
    op = ScreenOperation()
    request(op, "clipboard_set")
    with pytest.raises(ValueError):
        op.request(
            {
                "type": "screen_clipboard",
                "request_id": RID,
                "index": 0,
                "count": 1,
                "data": base64.b64encode(b"text").decode(),
                **change,
            }
        )


def test_only_fixed_operation_errors_and_explicit_requests():
    op = ScreenOperation()
    with pytest.raises(ValueError):
        op.response({"type": "screen_clipboard", "request_id": RID, "count": 1, "data": ""})
    request(op, "sas")
    with pytest.raises(ValueError):
        op.response(
            {
                "type": "screen_operation_result",
                "request_id": RID,
                "kind": "sas",
                "code": "secret clipboard text",
            }
        )
    op.response(
        {"type": "screen_operation_result", "request_id": RID, "kind": "sas", "code": "sas_blocked"}
    )
    assert not op.id


def test_cancel_race_drains_queued_replies_and_late_cancel_is_harmless():
    op = ScreenOperation()
    request(op, "clipboard_set")
    op.request(
        {"type": "screen_clipboard", "request_id": RID, "index": 0, "count": 1, "data": "eA=="}
    )
    op.request({"type": "screen_operation_cancel", "request_id": RID})
    op.response({"type": "screen_clipboard_ack", "request_id": RID, "index": 0})
    op.response(
        {
            "type": "screen_operation_result",
            "request_id": RID,
            "kind": "clipboard_set",
            "code": "ok",
        }
    )
    assert not op.id and not op.data
    op.request({"type": "screen_operation_cancel", "request_id": RID})


def test_late_clipboard_credit_at_timeout_does_not_poison_screen():
    op = ScreenOperation()
    request(op, "clipboard_get")
    op.response(
        {"type": "screen_clipboard", "request_id": RID, "index": 0, "count": 1, "data": "eA=="}
    )
    op.response(
        {
            "type": "screen_operation_result",
            "request_id": RID,
            "kind": "clipboard_get",
            "code": "operation_timeout",
        }
    )
    op.request({"type": "screen_clipboard_ack", "request_id": RID, "index": 0})
    assert not op.id
    with pytest.raises(ValueError):
        op.request({"type": "screen_clipboard_ack", "request_id": RID, "index": 2})
