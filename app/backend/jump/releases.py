"""Deterministic download links for official native agent releases."""

import re

REPOSITORY = "https://github.com/hotjared/jump"
ASSETS = {
    "linux": "jump-agent-linux-amd64",
    "windows": "jump-agent-windows-amd64.exe",
}
_TAG = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", re.ASCII)


def valid_release_tag(tag: str) -> bool:
    return _TAG.fullmatch(tag) is not None


def agent_downloads(server_version: str, agent_version: str = "") -> dict:
    version = agent_version or server_version
    if not valid_release_tag(version):
        return {"version": None, "downloads": {}, "checksums": None}
    base = f"{REPOSITORY}/releases/download/{version}"
    return {
        "version": version,
        "downloads": {platform: f"{base}/{name}" for platform, name in ASSETS.items()},
        "checksums": f"{base}/SHA256SUMS",
    }
