FROM node:22-alpine AS frontend
WORKDIR /src
COPY app/frontend/package*.json ./
RUN npm install
COPY app/frontend/ ./
RUN npm run build

FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
WORKDIR /opt/jump
COPY app/backend/pyproject.toml ./
COPY app/backend/jump ./jump
COPY app/backend/alembic ./alembic
COPY app/backend/alembic.ini ./
COPY --from=frontend /src/dist ./static
RUN pip install --no-cache-dir . && useradd -r -u 10001 jump
USER jump
EXPOSE 8000
CMD ["sh", "-c", "alembic upgrade head && exec uvicorn jump.main:app --host 0.0.0.0 --port 8000 --proxy-headers --forwarded-allow-ips='*'"]
