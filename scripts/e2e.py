"""Disposable local Compose verification. Requires Docker, Go and Internet access."""

import base64
import json
import os
import secrets
import subprocess
import tempfile
import time
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parent.parent


def run(args, *, env=None, capture=True):
    return (
        subprocess.run(
            args,
            cwd=ROOT,
            env=env,
            check=True,
            text=True,
            stdout=subprocess.PIPE if capture else None,
            stderr=subprocess.PIPE if capture else None,
        ).stdout
        or ""
    )


def wait_until(check, timeout=90):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            value = check()
            if value:
                return value
        except (OSError, subprocess.CalledProcessError, ValueError):
            pass
        time.sleep(2)
    raise AssertionError("Timed out waiting for stack state")


def main():
    with tempfile.TemporaryDirectory() as tmp:
        temp = Path(tmp)
        secrets_map = {
            "POSTGRES_PASSWORD": secrets.token_hex(32),
            "SESSION_SECRET": secrets.token_hex(32),
            "BROKER_INTERNAL_TOKEN": secrets.token_hex(32),
            "JUMP_MASTER_KEY": base64.b64encode(secrets.token_bytes(32)).decode(),
            "OIDC_CLIENT_SECRET": secrets.token_hex(32),
        }
        variables = {
            **secrets_map,
            "OIDC_ISSUER": "https://issuer.example",
            "OIDC_CLIENT_ID": "jump-test",
            "OIDC_REDIRECT_URI": "http://localhost:8000/auth/callback",
            "PUBLIC_URL": "http://localhost:8000",
            "AGENT_URL": "http://localhost:8080",
            "ALLOWED_HOSTS": "localhost,127.0.0.1,jump",
            "COOKIE_SECURE": "false",
            "JUMP_BIND_ADDRESS": "127.0.0.1",
        }
        envfile = temp / "compose.env"
        envfile.write_text("".join(f"{key}={value}\n" for key, value in variables.items()))
        env = {**os.environ, **variables}
        compose = [
            "docker",
            "compose",
            "-f",
            "docker-compose.yml",
            "-f",
            "docker-compose.dev.yml",
            "--env-file",
            str(envfile),
            "-p",
            "jump-e2e",
        ]

        def dc(*args):
            return run([*compose, *args], env=env)

        production = json.loads(
            run(
                [
                    "docker",
                    "compose",
                    "-f",
                    "docker-compose.yml",
                    "--env-file",
                    str(envfile),
                    "config",
                    "--format",
                    "json",
                ],
                env=env,
            )
        )
        services = production["services"]
        assert services["jump"]["image"] == "ghcr.io/hotjared/jump:latest"
        assert services["broker"]["image"] == "ghcr.io/hotjared/jump-broker:latest"
        assert all("build" not in service for service in services.values())
        assert all("ports" not in services[name] for name in ("postgres", "guacd"))
        published = {
            (name, str(port["published"]), int(port["target"]), port["host_ip"])
            for name, service in services.items()
            for port in service.get("ports", [])
        }
        assert published == {
            ("jump", "8000", 8000, "127.0.0.1"),
            ("broker", "8080", 8080, "127.0.0.1"),
        }
        assert services["jump"]["environment"]["BROKER_INTERNAL_URL"] == "http://broker:8081"
        assert all("internal" in services[name]["networks"] for name in services)

        def db_status():
            script = (
                "import json;"
                "from sqlalchemy import select,func,text;"
                "from jump.db import SessionLocal;"
                "from jump.models import AgentIdentity,Device;"
                "s=SessionLocal();"
                "d=s.scalar(select(Device).limit(1));"
                "print(json.dumps({'count':s.scalar(select(func.count(Device.id))),"
                "'online':d.online if d else False,"
                "'last_seen':d.last_seen_at.isoformat() if d and d.last_seen_at else None,"
                "'revoked':bool(s.scalar(select(AgentIdentity.revoked_at).where(AgentIdentity.device_id==d.id))) if d else False,"
                "'revision':s.execute(text('select version_num from alembic_version')).scalar()}))"
            )
            return json.loads(dc("exec", "-T", "jump", "python", "-c", script))

        agent = None
        agent_log = None
        try:
            dc("up", "-d", "--build")
            for image in ("jump:dev", "jump-broker:dev"):
                image_config = run(["docker", "image", "inspect", image], env=env)
                image_history = run(
                    [
                        "docker",
                        "image",
                        "history",
                        "--no-trunc",
                        "--format",
                        "{{.CreatedBy}}",
                        image,
                    ],
                    env=env,
                )
                for secret in secrets_map.values():
                    assert secret not in image_config + image_history, "secret baked into image"
            wait_until(lambda: urlopen("http://localhost:8000/health", timeout=3).status == 200)
            # Confirm that the private guacd service has its RDP protocol
            # handler available from the Jump container's network.
            guacd_check = (
                "import socket;"
                "s=socket.create_connection(('guacd',4822),timeout=3);"
                "s.settimeout(3);"
                "s.sendall(b'6.select,3.rdp;');"
                "reply=s.recv(512);"
                "assert reply.startswith(b'4.args,'),reply[:40];"
                "s.close()"
            )
            wait_until(lambda: dc("exec", "-T", "jump", "python", "-c", guacd_check) is not None)
            control_check = (
                "import os,urllib.request;"
                "r=urllib.request.Request("
                "'http://broker:8081/internal/devices/00000000-0000-0000-0000-000000000000/disconnect',"
                "data=b'',method='POST',"
                "headers={'Authorization':'Bearer '+os.environ['BROKER_INTERNAL_TOKEN']});"
                "assert urllib.request.urlopen(r,timeout=3).status==204"
            )
            dc("exec", "-T", "jump", "python", "-c", control_check)
            page = urlopen("http://localhost:8000/", timeout=3).read()
            assert b"<title>Jump</title>" in page
            assert db_status() == {
                "count": 0,
                "online": False,
                "last_seen": None,
                "revoked": False,
                "revision": "0004",
            }

            script = (
                "from jump.db import SessionLocal;"
                "from jump.security import map_oidc_user,create_enrollment;"
                "s=SessionLocal();"
                "u=map_oidc_user(s,'https://issuer.example',{'sub':'test-admin'});"
                "print(create_enrollment(s,u,'linux')[0])"
            )
            token = dc("exec", "-T", "jump", "python", "-c", script).strip()
            binary = temp / "jump-agent"
            subprocess.run(
                ["go", "mod", "tidy"],
                cwd=ROOT / "agent",
                env=env,
                check=True,
            )
            subprocess.run(
                ["go", "build", "-o", str(binary), "."],
                cwd=ROOT / "agent",
                env=env,
                check=True,
            )
            agent_env = env | {"JUMP_AGENT_STATE": str(temp / "identity.json")}
            run(
                [str(binary), "enroll", "--server", "http://localhost:8080", "--token", token],
                env=agent_env,
            )
            agent_log = (temp / "agent.log").open("w+")
            agent = subprocess.Popen(
                [str(binary), "run"],
                cwd=ROOT,
                env=agent_env,
                stdout=agent_log,
                stderr=agent_log,
            )
            wait_until(lambda: db_status()["count"] == 1 and db_status()["online"])
            first_seen = db_status()["last_seen"]
            time.sleep(22)
            later = db_status()
            assert later["online"] and later["last_seen"] > first_seen
            dc("restart", "jump", "broker")
            wait_until(lambda: db_status()["count"] == 1 and db_status()["online"])
            device_id = json.loads((temp / "identity.json").read_text())["device_id"]
            cookie_script = (
                "import os,base64,json;"
                "from itsdangerous import TimestampSigner;"
                "from jump.db import SessionLocal;"
                "from jump.models import User;"
                "from sqlalchemy import select;"
                "s=SessionLocal();"
                "u=s.scalar(select(User).where(User.oidc_subject=='test-admin'));"
                "state=base64.b64encode(json.dumps({'uid':str(u.id),'csrf':'e2e-csrf'}).encode());"
                "print(TimestampSigner(os.environ['SESSION_SECRET']).sign(state).decode())"
            )
            cookie = dc("exec", "-T", "jump", "python", "-c", cookie_script).strip()
            revoke_request = Request(
                f"http://localhost:8000/api/devices/{device_id}/revoke",
                data=b"",
                headers={
                    "Cookie": f"jump_session={cookie}",
                    "Origin": "http://localhost:8000",
                    "X-CSRF-Token": "e2e-csrf",
                },
                method="POST",
            )
            with urlopen(revoke_request, timeout=10) as response:
                assert response.status == 200
                assert json.load(response)["identity_state"] == "revoked"
            wait_until(lambda: db_status()["revoked"] and not db_status()["online"])
            assert db_status()["count"] == 1
            try:
                urlopen(f"http://localhost:8080/connect?device_id={device_id}", timeout=3)
            except HTTPError as exc:
                assert exc.code == 401, f"revoked identity returned {exc.code}"
            else:
                raise AssertionError("revoked identity was allowed to reconnect")
            logs = dc("logs", "--no-color", "jump", "broker")
            agent_log.flush()
            agent_log.seek(0)
            logs += agent_log.read()
            for secret in secrets_map.values():
                assert secret not in logs, "secret appeared in container logs"
            for marker in ("Traceback", "panic:", "migration error", '"level":"ERROR"'):
                if marker in logs:
                    matches = [line for line in logs.splitlines() if marker in line]
                    diagnostic = "\n".join(matches[-20:])
                    for secret in (*secrets_map.values(), token):
                        diagnostic = diagnostic.replace(secret, "[REDACTED]")
                    raise AssertionError(f"unexpected log marker: {marker}\n{diagnostic}")
            warnings = [line for line in logs.splitlines() if '"level":"WARN"' in line]
            print(f"Structured warning lines during restart: {len(warnings)}")
            print(
                "Compose, migration, enrollment, heartbeat, restart, revocation and persistence: passed"
            )
        finally:
            if agent:
                agent.terminate()
                try:
                    agent.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    agent.kill()
            if agent_log:
                agent_log.close()
            dc("down", "-v", "--remove-orphans")


if __name__ == "__main__":
    main()
