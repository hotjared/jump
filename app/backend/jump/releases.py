"""Discover and validate official native agent releases."""

import re
import time
from threading import Lock

import httpx

REPOSITORY = "https://github.com/hotjared/jump"
RELEASES_API = "https://api.github.com/repos/hotjared/jump/releases?per_page=100"
ASSETS = {
    "linux": "jump-agent-linux-amd64",
    "windows": "jump-agent-windows-amd64.exe",
}
_TAG = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", re.ASCII)
_STABLE = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", re.ASCII)
_SHA = re.compile(r"[a-fA-F0-9]{64}", re.ASCII)
_cache: dict[str, tuple[float, dict[str, str]]] = {}
_lock = Lock()
_latest_lock = Lock()
_latest_version: str | None = None
_latest_expires = 0.0


def latest_agent_release() -> str | None:
    """Cache the newest usable stable release, retaining a validated value on API failure."""
    global _latest_version, _latest_expires
    with _latest_lock:
        current_time = time.monotonic()
        if current_time < _latest_expires:
            return _latest_version
        try:
            with httpx.Client(timeout=5, follow_redirects=False) as client:
                response = client.get(
                    RELEASES_API, headers={"Accept": "application/vnd.github+json"}
                )
                response.raise_for_status()
                if len(response.content) > 1_000_000:
                    raise ValueError("Release listing too large")
                listing = response.json()
            if not isinstance(listing, list):
                raise ValueError("Invalid release listing")
            required = {*ASSETS.values(), "SHA256SUMS"}
            candidates = []
            for release in listing:
                if (
                    not isinstance(release, dict)
                    or release.get("draft") is not False
                    or release.get("prerelease") is not False
                ):
                    continue
                tag = release.get("tag_name")
                number = release_number(tag) if isinstance(tag, str) else None
                assets = release.get("assets")
                if number is None or not isinstance(assets, list):
                    continue
                names = {
                    asset.get("name")
                    for asset in assets
                    if isinstance(asset, dict)
                    and isinstance(asset.get("name"), str)
                    and asset.get("state") == "uploaded"
                    and isinstance(asset.get("size"), int)
                    and asset["size"] > 0
                }
                if required <= names:
                    candidates.append((number, tag))
            # A successful empty listing is not a temporary failure: there is no usable target.
            _latest_version = max(candidates)[1] if candidates else None
            _latest_expires = time.monotonic() + 300
        except (httpx.HTTPError, ValueError, TypeError):
            # Back off on failures too, so an unavailable API cannot stall every request.
            _latest_expires = time.monotonic() + 60
        return _latest_version


def valid_release_tag(tag: str) -> bool:
    return _TAG.fullmatch(tag) is not None


def release_number(tag: str | None) -> tuple[int, int, int] | None:
    """Only stable published versions are eligible for unattended execution."""
    match = _STABLE.fullmatch(tag) if isinstance(tag, str) else None
    return tuple(map(int, match.groups())) if match else None


def newer_release(current: str, target: str | None) -> bool:
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


def agent_downloads(version: str | None) -> dict:
    if version is None or release_number(version) is None:
        return {"version": None, "downloads": {}, "checksums": None}
    base = f"{REPOSITORY}/releases/download/{version}"
    return {
        "version": version,
        "downloads": {platform: f"{base}/{name}" for platform, name in ASSETS.items()},
        "checksums": f"{base}/SHA256SUMS",
    }
