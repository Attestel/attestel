# syntax=docker/dockerfile:1
# ─────────────────────────────────────────────────────────────────────────────
# Single all-in-one image for a single-container PaaS deploy: 4 Python (FastAPI) + 6 Go services + the
# built React app, fronted by nginx on one public port, supervised by supervisord.
# No docker-compose required.
# ─────────────────────────────────────────────────────────────────────────────

# ---- Stage 1: build the 6 Go services (static binaries) ----
FROM golang:1.25-alpine AS gobuild
WORKDIR /src
COPY gateway/ ./gateway/
COPY alerts/  ./alerts/
COPY journal/ ./journal/
COPY paper/   ./paper/
COPY auth/    ./auth/
COPY feedback/ ./feedback/
RUN set -eux; \
    cd /src/gateway && CGO_ENABLED=0 go build -trimpath -o /out/gateway .; \
    cd /src/alerts  && CGO_ENABLED=0 go build -trimpath -o /out/alerts  .; \
    cd /src/journal && CGO_ENABLED=0 go build -trimpath -o /out/journal .; \
    cd /src/paper   && CGO_ENABLED=0 go build -trimpath -o /out/paper   .; \
    cd /src/auth    && CGO_ENABLED=0 go build -trimpath -o /out/auth    .; \
    cd /src/feedback && CGO_ENABLED=0 go build -trimpath -o /out/feedback .

# ---- Stage 2: build the React app (same-origin API URLs baked in) ----
FROM node:20-alpine AS webbuild
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# Point the browser at same-origin paths that nginx proxies (see nginx.conf.template).
# Empty VITE_GATEWAY_URL => same origin => calls hit /api and /health. VITE_AUTH_URL=/svc/auth so the
# auth calls (authJSON adds /auth/...) resolve to /svc/auth/auth/... which nginx strips to /auth/...
RUN printf 'VITE_GATEWAY_URL=\nVITE_ALERTS_URL=/svc/alerts\nVITE_JOURNAL_URL=/svc/journal\nVITE_PAPER_URL=/svc/paper\nVITE_FEEDBACK_URL=/svc/feedback\nVITE_AUTH_URL=/svc/auth\n' > .env.production
RUN npm run build

# ---- Stage 3: runtime — Python base + nginx + supervisor + Go binaries + web ----
FROM python:3.11-slim AS runtime
# MALLOC_ARENA_MAX=2: glibc defaults to 8 arenas per core — with 3 long-running pandas/numpy
# services under constant polling (alerts 60s / paper 5m / quote 20s) that fragments into GBs of
# un-returned RSS over days. Two arenas is the standard container fix; throughput cost is negligible.
ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    MALLOC_ARENA_MAX=2
# nginx (proxy/static), supervisor (process mgr), libgomp1 (LightGBM runtime), curl (healthcheck)
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends nginx supervisor libgomp1 curl ca-certificates; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Python deps (union of all four services — identical pins, no conflicts)
COPY deploy/requirements-all.txt /app/deploy/requirements-all.txt
RUN pip install -r /app/deploy/requirements-all.txt
COPY deploy/requirements-postgres.txt /app/deploy/requirements-postgres.txt
RUN pip install -r /app/deploy/requirements-postgres.txt

# App code + artifacts
COPY services/ /app/services/
COPY --from=gobuild /out/           /app/bin/
# Public web root: "/" is the public entry page and the React product lives under "/app/".
# The Vite build is emitted with base=/app/, so its chunks resolve at /app/assets/...
# (see web/vite.config.js and deploy/nginx.conf.template). Entry-page assets live at /assets/.
COPY --from=webbuild /web/dist/     /var/www/html/app/
# "/" is a minimal public entry page that hands off to the React product at /app/. The marketing
# site this repo was extracted from is not part of the open-source release; drop your own
# index.html (plus /assets) into deploy/webroot/ to serve one here.
COPY deploy/webroot/                /var/www/html/
COPY deploy/nginx.conf.template deploy/supervisord.conf deploy/entrypoint.sh deploy/fatalwatch.py /app/deploy/
RUN chmod +x /app/deploy/entrypoint.sh /app/deploy/fatalwatch.py /app/bin/*
# COPY preserves the source file mode, and a web-root file checked out 0600 would make nginx
# workers (which drop to www-data) 403 the homepage. Normalise the web root (a+rX = readable
# files, traversable dirs) and drop the distro's default placeholder page, which is otherwise
# reachable at /index.nginx-debian.html.
RUN chmod -R a+rX /var/www/html \
 && rm -f /var/www/html/index.nginx-debian.html

# File fallback/cache/legacy-import locations. Durable application records use DATABASE_URL in
# production; credentials are runtime-only.
ENV MODELS_DIR=/data/models \
    EVAL_OUT_DIR=/data/eval \
    READS_DIR=/data/reads \
    RULES_DIR=/data/alerts \
    TRADES_DIR=/data/journal \
    DATA_DIR=/data/paper \
    FEEDBACK_DIR=/data/feedback \
    USERS_DIR=/data/auth \
    BARS_DATABASE_SCHEMA=analysis

# Public port (overridden by $PORT at runtime). Gateway is internal on 8181.
EXPOSE 8080

# ⚠ KUBERNETES IGNORES HEALTHCHECK ENTIRELY. On such a platform this instruction is documentation, not
# enforcement — only livenessProbe / readinessProbe / startupProbe in the pod spec do anything:
#
#   startupProbe:   httpGet /ready  · failureThreshold 40 · periodSeconds 10   (gives boot ~400s)
#   readinessProbe: httpGet /ready  · periodSeconds 10                          (503 ⇒ out of rotation)
#   livenessProbe:  httpGet /health · periodSeconds 20                          (200 unless the gateway is wedged)
#
# Point liveness at /health and NOT /ready, or a PostgreSQL outage restart-loops a healthy pod.
#
# The check below is what remains for plain `docker run`. It is now the gateway's own deep probe
# rather than four shallow curls: `/ready` fans out to the upstreams AND inspects their database
# blocks, so it fails on a dead PostgreSQL — which the previous version could not do, because
# analysis and events both answer 200 with their store down. (llm:8002 is still excluded: it fails
# soft to the deterministic stub, so READY_REQUIRE leaves it out.)
HEALTHCHECK --interval=15s --timeout=5s --start-period=60s --retries=5 \
  CMD curl -fsS -o /dev/null http://127.0.0.1:8181/ready || exit 1

ENTRYPOINT ["/app/deploy/entrypoint.sh"]
