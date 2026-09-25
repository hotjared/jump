from jump.config import Settings
from jump.releases import agent_downloads


def test_download_links_use_server_release_and_fixed_asset_names():
    links = agent_downloads("v1.2.3")
    assert links == {
        "version": "v1.2.3",
        "downloads": {
            "linux": "https://github.com/hotjared/jump/releases/download/v1.2.3/jump-agent-linux-amd64",
            "windows": "https://github.com/hotjared/jump/releases/download/v1.2.3/jump-agent-windows-amd64.exe",
        },
        "checksums": "https://github.com/hotjared/jump/releases/download/v1.2.3/SHA256SUMS",
    }


def test_development_override_and_untrusted_tag():
    assert agent_downloads("dev")["downloads"] == {}
    assert agent_downloads("dev", "v2.0.0")["version"] == "v2.0.0"
    assert agent_downloads("v1.0.0", "v2.0.0")["version"] == "v2.0.0"
    for value in ("latest", "https://evil.test", "v1.2.3/other", "v1.2.3?token=secret"):
        assert agent_downloads(value)["downloads"] == {}


def test_invalid_override_fails_configuration_validation():
    settings = Settings(jump_agent_version="v1.2.3/other")
    try:
        settings.validate_production()
    except RuntimeError as exc:
        assert "JUMP_AGENT_VERSION" in str(exc)
    else:
        raise AssertionError("unsafe override was accepted")
