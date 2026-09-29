from unittest.mock import Mock

import httpx

from jump import releases
from jump.config import Settings
from jump.releases import agent_downloads, latest_agent_release


def release(tag, *, draft=False, prerelease=False, assets=None):
    return {
        "tag_name": tag,
        "draft": draft,
        "prerelease": prerelease,
        "assets": [
            {"name": name, "state": "uploaded", "size": 42}
            for name in (
                assets
                if assets is not None
                else ["jump-agent-linux-amd64", "jump-agent-windows-amd64.exe", "SHA256SUMS"]
            )
        ],
    }


def test_latest_usable_stable_release_and_cache(monkeypatch):
    monkeypatch.setattr(releases, "_latest_version", None)
    monkeypatch.setattr(releases, "_latest_expires", 0)
    clock = [1000.0]
    monkeypatch.setattr(releases.time, "monotonic", lambda: clock[0])
    calls = []
    payload = [
        release("v1.0.0"),
        release("v99.0.0", draft=True),
        release("v98.0.0", prerelease=True),
        release("v3.0.0-rc.1"),
        release("v04.0.0"),
        release("v2.0.0", assets=["SHA256SUMS"]),
        release("v1.2.3", assets=["jump-agent-linux-amd64", "jump-agent-windows-amd64.exe"]),
        release("v1.1.0"),
    ]

    def get(self, url, **kwargs):
        calls.append(url)
        return Mock(content=b"[]", json=lambda: payload, raise_for_status=lambda: None)

    monkeypatch.setattr(httpx.Client, "get", get)
    assert latest_agent_release() == "v1.1.0"
    assert latest_agent_release() == "v1.1.0"
    assert calls == [releases.RELEASES_API]
    payload[:] = [release("v1.3.0")]
    clock[0] += 301
    assert latest_agent_release() == "v1.3.0"
    assert len(calls) == 2

    def unavailable(self, url, **kwargs):
        calls.append(url)
        raise httpx.ConnectError("offline")

    monkeypatch.setattr(httpx.Client, "get", unavailable)
    clock[0] += 301
    assert latest_agent_release() == "v1.3.0"
    assert latest_agent_release() == "v1.3.0"
    assert len(calls) == 3


def test_no_validated_release_fails_safely_and_retries(monkeypatch):
    monkeypatch.setattr(releases, "_latest_version", None)
    monkeypatch.setattr(releases, "_latest_expires", 0)
    monkeypatch.setattr(
        httpx.Client,
        "get",
        lambda self, url, **kwargs: (_ for _ in ()).throw(httpx.ConnectError("offline")),
    )
    assert latest_agent_release() is None
    assert agent_downloads(latest_agent_release()) == {
        "version": None,
        "downloads": {},
        "checksums": None,
    }


def test_download_links_use_validated_tag_and_fixed_asset_names():
    links = agent_downloads("v1.2.3")
    assert links == {
        "version": "v1.2.3",
        "downloads": {
            "linux": "https://github.com/hotjared/jump/releases/download/v1.2.3/jump-agent-linux-amd64",
            "windows": "https://github.com/hotjared/jump/releases/download/v1.2.3/jump-agent-windows-amd64.exe",
        },
        "checksums": "https://github.com/hotjared/jump/releases/download/v1.2.3/SHA256SUMS",
    }
    for value in (None, "dev", "v1.2.3-rc.1", "https://evil.test", "v1.2.3/other"):
        assert agent_downloads(value)["downloads"] == {}


def test_manual_agent_version_is_not_a_setting(monkeypatch):
    monkeypatch.setenv("JUMP_AGENT_VERSION", "v99.0.0")
    assert "jump_agent_version" not in Settings.model_fields
    assert not hasattr(Settings(), "jump_agent_version")
