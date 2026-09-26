"""Deterministic download links for official native agent releases."""

import re
import time
from threading import Lock

import httpx

REPOSITORY = "https://github.com/hotjared/jump"
ASSETS = {
    "linux": "jump-agent-linux-amd64",
    "windows": "jump-agent-windows-amd64.exe",
}
_TAG = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", re.ASCII)
_STABLE = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", re.ASCII)
_SHA = re.compile(r"[a-fA-F0-9]{64}", re.ASCII)
_cache: dict[str, tuple[float, dict[str, str]]] = {}
_lock = Lock()


def valid_release_tag(tag: str) -> bool:
    return _TAG.fullmatch(tag) is not None


def release_number(tag: str) -> tuple[int, int, int] | None:
    """Only stable published versions are eligible for unattended execution."""
    match = _STABLE.fullmatch(tag)
    return tuple(map(int, match.groups())) if match else None


def newer_release(current: str, target: str) -> bool:
    old, new = release_number(current), release_number(target)
    return old is not None and new is not None and new > old


def release_asset(version: str, platform: str) -> dict[str, str]:
    """Resolve only a configured official asset; cache the bounded checksum lookup."""
    if release_number(version) is None or platform not in ASSETS:
        raise ValueError("Unsupported release")
    with _lock:
        cached = _cache.get(version)
        if cached and cached[0] > time.monotonic():
            sums = cached[1]
        else:
            base = f"{REPOSITORY}/releases/download/{version}"
            with httpx.Client(timeout=8, follow_redirects=False) as client:
                address = base + "/SHA256SUMS"
                for _ in range(3):
                    parsed = httpx.URL(address)
                    if (
                        parsed.scheme != "https"
                        or parsed.host not in ("github.com", "release-assets.githubusercontent.com")
                        or parsed.port not in (None, 443)
                    ):
                        raise ValueError("Untrusted release metadata")
                    response = client.get(address)
                    if response.status_code not in (301, 302, 303, 307, 308):
                        break
                    address = str(response.url.join(response.headers["location"]))
                else:
                    raise ValueError("Too many release redirects")
                response.raise_for_status()
                if len(response.content) > 16_384:
                    raise ValueError("Release metadata too large")
            sums = {}
            for line in response.text.splitlines():
                match = re.fullmatch(
                    r"([a-fA-F0-9]{64})  (jump-agent-(?:linux-amd64|windows-amd64\.exe))", line
                )
                if match and match[2] not in sums:
                    sums[match[2]] = match[1].lower()
            _cache[version] = (time.monotonic() + 300, sums)
    asset = ASSETS[platform]
    if asset not in sums or not _SHA.fullmatch(sums[asset]):
        raise ValueError("Release checksum unavailable")
    return {
        "version": version,
        "platform": platform,
        "architecture": "amd64",
        "download_url": f"{REPOSITORY}/releases/download/{version}/{asset}",
        "sha256": sums[asset],
    }


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
