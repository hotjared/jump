import base64
from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict

from .releases import valid_release_tag


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    database_url: str = "postgresql+psycopg://jump:jump@localhost:5432/jump"
    oidc_issuer: str = ""
    oidc_client_id: str = ""
    oidc_client_secret: str = ""
    oidc_redirect_uri: str = ""
    public_url: str = "http://localhost:8000"
    agent_url: str = "http://localhost:8080"
    broker_internal_url: str = "http://broker:8081"
    session_secret: str = ""
    broker_internal_token: str = ""
    jump_master_key: str = ""
    cookie_secure: bool = True
    allowed_hosts: str = "localhost,127.0.0.1,jump"
    jump_server_version: str = "dev"
    jump_agent_version: str = ""

    def validate_production(self) -> None:
        if self.jump_agent_version and not valid_release_tag(self.jump_agent_version):
            raise RuntimeError("JUMP_AGENT_VERSION must be a version tag such as v0.1.0")
        for name in (
            "oidc_issuer",
            "oidc_client_id",
            "oidc_client_secret",
            "oidc_redirect_uri",
            "session_secret",
            "broker_internal_token",
            "jump_master_key",
        ):
            if not getattr(self, name):
                raise RuntimeError(f"{name.upper()} must be configured")
            if getattr(self, name).startswith("REPLACE_"):
                raise RuntimeError(f"{name.upper()} still contains a placeholder")
        if len(self.session_secret) < 32 or len(self.broker_internal_token) < 32:
            raise RuntimeError("Session and broker secrets must have at least 32 characters")
        try:
            key = base64.b64decode(self.jump_master_key, validate=True)
        except ValueError as exc:
            raise RuntimeError("JUMP_MASTER_KEY must be base64 encoded") from exc
        if len(key) != 32:
            raise RuntimeError("JUMP_MASTER_KEY must encode exactly 32 bytes")


@lru_cache
def settings() -> Settings:
    return Settings()
