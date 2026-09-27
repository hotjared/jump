import uuid
from datetime import datetime
from ipaddress import ip_address
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, field_validator


class DevicePatch(BaseModel):
    display_name: str | None = Field(default=None, max_length=255)
    group_id: uuid.UUID | None = None
    tag_ids: list[uuid.UUID] = Field(default_factory=list, max_length=50)


class NameInput(BaseModel):
    name: str = Field(min_length=1, max_length=100)


class EnrollmentInput(BaseModel):
    os_family: str


class Metadata(BaseModel):
    model_config = ConfigDict(extra="forbid")

    hostname: str = Field(min_length=1, max_length=255)
    os_family: Literal["linux", "windows"]
    os_version: str = Field(default="", max_length=255)
    architecture: str = Field(default="", max_length=32)
    agent_version: str = Field(default="", max_length=64)
    capabilities: list[str] = Field(default_factory=list, max_length=32)
    addresses: list[str] = Field(default_factory=list, max_length=16)
    current_user: str | None = Field(default=None, max_length=255)

    @field_validator("addresses")
    @classmethod
    def valid_addresses(cls, value: list[str]) -> list[str]:
        for address in value:
            ip_address(address)
        return value

    @field_validator("capabilities")
    @classmethod
    def valid_capabilities(cls, value: list[str]) -> list[str]:
        if any(len(item) > 64 or not item.isascii() for item in value):
            raise ValueError("Invalid capability")
        return value


class EnrollRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")
    token: str = Field(min_length=30, max_length=128)
    public_key: str
    metadata: Metadata


class PresenceInput(BaseModel):
    model_config = ConfigDict(extra="forbid")
    connection_id: str = Field(max_length=36)
    metadata: Metadata | None = None


class AuditOutput(BaseModel):
    id: uuid.UUID
    event_type: str
    actor_user_id: uuid.UUID | None
    device_id: uuid.UUID | None
    detail: dict
    created_at: datetime

    model_config = {"from_attributes": True}


class CredentialInput(BaseModel):
    label: str = Field(min_length=1, max_length=255)
    kind: Literal["linux_password", "linux_ssh_key", "windows_password"]
    username: str = Field(min_length=1, max_length=255)
    secret: str = Field(min_length=1, max_length=16384)
    domain: str | None = Field(default=None, max_length=255)


class SSHSessionInput(BaseModel):
    credential_id: uuid.UUID
    columns: int = Field(ge=20, le=500)
    rows: int = Field(ge=5, le=200)


class RDPSessionInput(BaseModel):
    credential_id: uuid.UUID
    width: int = Field(ge=320, le=7680)
    height: int = Field(ge=200, le=4320)
    dpi: int = Field(default=96, ge=72, le=300)
