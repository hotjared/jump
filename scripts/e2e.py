"""Disposable local Compose verification. Requires Docker, Go and Internet access."""

import base64
import json
import os
import secrets
import subprocess
import tempfile
import time
from pathlib import Path
from urllib.request import urlopen

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
        compose = ["docker", "compose", "--env-file", str(envfile), "-p", "jump-e2e"]

        def dc(*args):
            return run([*compose, *args], env=env)

        def db_status():
            script = (
                "import json;"
                "from sqlalchemy import select,func,text;"
                "from jump.db import SessionLocal;"
                "from jump.models import Device;"
                "s=SessionLocal();"
                "d=s.scalar(select(Device).limit(1));"
                "print(json.dumps({'count':s.scalar(select(func.count(Device.id))),"
                "'online':d.online if d else False,"
                "'revision':s.execute(text('select version_num from alembic_version')).scalar()}))"
            )
            return json.loads(dc("exec", "-T", "jump", "python", "-c", script))

        agent = None
        try:
            dc("up", "-d", "--build")
            wait_until(lambda: urlopen("http://localhost:8000/health", timeout=3).status == 200)
            page = urlopen("http://localhost:8000/", timeout=3).read()
            assert b"<title>Jump</title>" in page
            assert db_status() == {"count": 0, "online": False, "revision": "0001"}

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
                ["go", "build", "-o", str(binary), "."],
                cwd=ROOT / "agent",
                env=env,
                check=True,
                capture_output=True,
            )
            agent_env = env | {"JUMP_AGENT_STATE": str(temp / "identity.json")}
            run(
                [str(binary), "enroll", "--server", "http://localhost:8080", "--token", token],
                env=agent_env,
            )
            agent = subprocess.Popen(
                [str(binary), "run"],
                cwd=ROOT,
                env=agent_env,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            wait_until(lambda: db_status()["count"] == 1 and db_status()["online"])
            time.sleep(22)
            assert db_status()["online"]  # Heartbeat across the normal interval.
            dc("restart", "jump", "broker")
            wait_until(lambda: db_status()["count"] == 1 and db_status()["online"])
            logs = dc("logs", "--no-color", "jump", "broker")
            for secret in secrets_map.values():
                assert secret not in logs, "secret appeared in container logs"
            assert "Traceback" not in logs and "panic:" not in logs
            print("Compose, migration, UI, enrollment, heartbeat, restart and persistence: passed")
        finally:
            if agent:
                agent.terminate()
                try:
                    agent.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    agent.kill()
            dc("down", "-v", "--remove-orphans")


if __name__ == "__main__":
    main()
