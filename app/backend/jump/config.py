import base64
from functools import lru_cache

from pydantic import field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    database_url: str = "postgresql+psycopg://jump:jump@localhost:5432/jump"
    auth_mode: str = "oidc"
    oidc_issuer: str = ""
    oidc_client_id: str = ""
    oidc_client_secret: str = ""
    oidc_redirect_uri: str = ""
    public_url: str = "http://localhost:8000"
    agent_url: str = "http://localhost:8080"
    broker_internal_url: str = "http://broker:8081"
    guacd_host: str = "guacd"
    guacd_port: int = 4822
    rdp_bridge_host: str = "jump"
    session_secret: str = ""
    broker_internal_token: str = ""
    jump_master_key: str = ""
    cookie_secure: bool = True
    allowed_hosts: str = "localhost,127.0.0.1,jump"
    jump_server_version: str = "dev"
    ssh_idle_seconds: int = 1800

    @field_validator("auth_mode")
    @classmethod
    def valid_auth_mode(cls, value: str) -> str:
        if value not in ("oidc", "local", "hybrid"):
            raise ValueError("AUTH_MODE must be oidc, local, or hybrid")
        return value

    @property
    def oidc_enabled(self) -> bool:
        return self.auth_mode in ("oidc", "hybrid")

    @property
    def local_enabled(self) -> bool:
        return self.auth_mode in ("local", "hybrid")

    def validate_production(self) -> None:
        names = (
            "session_secret",
            "broker_internal_token",
            "jump_master_key",
        )
        if self.oidc_enabled:
            names = (
                "oidc_issuer",
                "oidc_client_id",
                "oidc_client_secret",
                "oidc_redirect_uri",
            ) + names
        for name in names:
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
