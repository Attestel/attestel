#!/bin/sh
# ─────────────────────────────────────────────────────────────────────────────────────────────────
# Fail-fast entrypoint for the all-in-one image.
#
# WHAT THIS EXISTS TO PREVENT.
#
# On 2026-08-22 this container reported a fully successful deploy while the events service was not
# running at all. Nothing lied: supervisord started its programs, one exited, supervisord retried it
# ten times and marked it FATAL, and the container carried on serving because nginx and supervisord
# were both alive. Kubernetes saw a running pod. The Docker HEALTHCHECK that was supposed to catch
# this is inert under Kubernetes, which ignores HEALTHCHECK entirely.
#
# So the container now refuses to start when the IMAGE is broken, and refuses to finish starting
# when a service cannot come up. Two gates, in order:
#
#   1. PREFLIGHT    — deterministic, own-fault checks needing no network: are the binaries there and
#                     executable, does every Python module compile, do the third-party imports
#                     resolve, does the generated nginx config parse, is required configuration
#                     present. Any failure exits 1 BEFORE supervisord starts. None of these is ever
#                     fixed by a restart, and a CrashLoopBackOff naming the reason beats a green
#                     deploy hiding it.
#
#   2. STARTUP GATE — supervisord runs in the background while we wait for every service to actually
#                     answer on its port. A program that dies, or never listens, fails the deploy.
#
# POSTGRESQL FAILURE POSTURE. Read-only/provider features may degrade, but experiment-critical state
# may not silently fall back to container files once DATABASE_URL is configured. Prediction reports
# 503 and llm/paper/journal/alerts/feedback/auth fail closed if their store cannot be opened. `/ready`
# (gateway/health.go) removes the pod from rotation; persistent failure can therefore restart the
# container, but it can never create a second, disposable paper book behind the operator's back.
# ─────────────────────────────────────────────────────────────────────────────────────────────────
set -eu

log()  { echo "nvda-platform: $*"; }
fail() { echo "nvda-platform: PREFLIGHT FAILED: $*" >&2; exit 1; }

# nginx always listens on 8080 (set your platform's service targetPort to 8080). If the platform injects a
# different $PORT, add it as an extra listener so either routing contract works. Gateway is internal
# on 8181, so there's never a collision.
PORT="${PORT:-8080}"
if [ "$PORT" = "8080" ]; then
  EXTRA_LISTEN=""
else
  EXTRA_LISTEN="    listen      ${PORT};"
fi
sed "s|__EXTRA_LISTEN__|${EXTRA_LISTEN}|" /app/deploy/nginx.conf.template > /etc/nginx/nginx.conf

# ── 1. PREFLIGHT ────────────────────────────────────────────────────────────────────────────────
log "preflight: checking the image can actually run"

# 1a. Required configuration. PostgreSQL is a required external dependency; its REACHABILITY is not
# checked here (see the header), but its ABSENCE is a configuration error provable instantly.
POSTGRES_URL="${DATABASE_URL:-${EVENTS_DATABASE_URL:-${BARS_DATABASE_URL:-}}}"
[ -n "${POSTGRES_URL}" ] || fail "DATABASE_URL (or a service-specific PostgreSQL URL) is required"
case "${POSTGRES_URL}" in
  postgresql://*|postgres://*) ;;
  *) fail "DATABASE_URL must use postgresql:// or postgres:// (got '${POSTGRES_URL%%:*}')" ;;
esac
export EVENTS_DATABASE_URL="${EVENTS_DATABASE_URL:-${POSTGRES_URL}}"
export BARS_DATABASE_URL="${BARS_DATABASE_URL:-${POSTGRES_URL}}"
export PREDICTION_DATABASE_URL="${PREDICTION_DATABASE_URL:-${POSTGRES_URL}}"
export LLM_DATABASE_URL="${LLM_DATABASE_URL:-${POSTGRES_URL}}"
export JOURNAL_DATABASE_URL="${JOURNAL_DATABASE_URL:-${POSTGRES_URL}}"
export PAPER_DATABASE_URL="${PAPER_DATABASE_URL:-${POSTGRES_URL}}"
export ALERTS_DATABASE_URL="${ALERTS_DATABASE_URL:-${POSTGRES_URL}}"
export FEEDBACK_DATABASE_URL="${FEEDBACK_DATABASE_URL:-${POSTGRES_URL}}"
export AUTH_DATABASE_URL="${AUTH_DATABASE_URL:-${POSTGRES_URL}}"

# A warning, not a failure: the shipped default works, and refusing to boot over it would break
# every local run. In a deployed image it still means every session cookie is forgeable.
case "${AUTH_SECRET:-}" in
  ""|dev-insecure-change-me)
    echo "nvda-platform: WARNING: AUTH_SECRET is unset or the dev default — session cookies are forgeable" >&2 ;;
esac

# 1b. The six Go binaries. A build stage that silently produced nothing is the cheapest failure to
# detect and among the most expensive to diagnose from a running container.
for bin in gateway alerts journal paper feedback auth; do
  [ -f "/app/bin/${bin}" ] || fail "missing Go binary /app/bin/${bin} — the gobuild stage did not produce it"
  [ -x "/app/bin/${bin}" ] || fail "/app/bin/${bin} is not executable"
done

# 1c. The four Python services exist and COMPILE. compileall is syntax-only and costs ~1s; a genuine
# ImportError is left to the startup gate below, because importing app.main here would pay the ~15s
# pandas/numpy/sklearn/lightgbm cost twice on every boot.
for svc in analysis llm prediction events; do
  [ -f "/app/services/${svc}/app/main.py" ] || fail "missing /app/services/${svc}/app/main.py"
done
python3 -m compileall -q /app/services >/dev/null \
  || fail "a Python service does not compile (SyntaxError above)"

# 1d. Third-party imports resolve. Catches a dependency that failed to install into the runtime
# layer — which otherwise surfaces as four services crash-looping with an ImportError nobody reads.
python3 - <<'PY' || fail "a required Python dependency is missing from the image"
import importlib, sys
missing = []
for mod in ("fastapi", "uvicorn", "pydantic", "requests", "numpy", "pandas",
            "sklearn", "lightgbm", "psycopg"):
    try:
        importlib.import_module(mod)
    except Exception as exc:
        missing.append(f"{mod}: {type(exc).__name__}: {exc}")
if missing:
    print("nvda-platform: unimportable dependencies:", file=sys.stderr)
    for line in missing:
        print("  -", line, file=sys.stderr)
    sys.exit(1)
PY

# 1e. The generated nginx config parses. `sed` on a template is exactly the kind of step that
# produces a valid-looking file nginx then refuses at runtime.
nginx -t -c /etc/nginx/nginx.conf >/dev/null 2>&1 \
  || { nginx -t -c /etc/nginx/nginx.conf || true; fail "generated /etc/nginx/nginx.conf does not parse"; }

log "preflight: OK"

# Cache/fallback/legacy-import dirs. Experiment-critical state uses PostgreSQL in production.
mkdir -p /data/models /data/reads /data/alerts /data/journal /data/paper /data/eval /data/feedback /data/auth \
  /data

# ── 2. STARTUP GATE ─────────────────────────────────────────────────────────────────────────────
#
# supervisord runs in the background so this script can watch it. Once every service answers we hand
# over: supervisord stays the container's only long-lived process and this shell waits on it, so its
# exit status — including the one fatalwatch.py forces — becomes the container's.
#
# STARTUP_GATE_SECONDS must exceed EVENTS_DB_BOOT_RETRY_SECONDS (default 120): with PostgreSQL down
# the events service deliberately spends that budget retrying before it serves degraded, and that is
# a slow boot, not a failed one.
STARTUP_GATE_SECONDS="${STARTUP_GATE_SECONDS:-210}"

log "all-in-one container starting, public port ${PORT}"
supervisord -c /app/deploy/supervisord.conf &
SUPERVISORD_PID=$!

# Every service, as "name:port". nginx is included on $PORT — it is the only one whose readiness the
# outside world can observe, so a deploy that leaves it silent is a failed deploy.
PENDING="analysis:8001 llm:8002 prediction:8003 events:8004 gateway:8181 alerts:8095 journal:8096 paper:8097 feedback:8098 auth:8099 nginx:${PORT}"

gate_deadline=$(( $(date +%s) + STARTUP_GATE_SECONDS ))

while [ -n "${PENDING}" ]; do
  # supervisord dying during startup is terminal — there is nothing left to wait for.
  if ! kill -0 "${SUPERVISORD_PID}" 2>/dev/null; then
    echo "nvda-platform: STARTUP FAILED: supervisord exited during startup" >&2
    wait "${SUPERVISORD_PID}" || exit $?
    exit 1
  fi

  still_pending=""
  for entry in ${PENDING}; do
    name="${entry%%:*}"
    port="${entry##*:}"
    if curl -fsS -m 3 -o /dev/null "http://127.0.0.1:${port}/health" 2>/dev/null; then
      log "startup: ${name} answering on ${port}"
    else
      still_pending="${still_pending} ${entry}"
    fi
  done
  PENDING="$(echo "${still_pending}" | sed 's/^ *//')"
  [ -n "${PENDING}" ] || break

  if [ "$(date +%s)" -ge "${gate_deadline}" ]; then
    echo "nvda-platform: STARTUP FAILED: still not answering after ${STARTUP_GATE_SECONDS}s: ${PENDING}" >&2
    echo "nvda-platform: supervisord state at failure:" >&2
    supervisorctl -c /app/deploy/supervisord.conf status >&2 2>&1 || true
    kill -TERM "${SUPERVISORD_PID}" 2>/dev/null || true
    wait "${SUPERVISORD_PID}" 2>/dev/null || true
    exit 1
  fi
  sleep 2
done

log "startup: all services answering — handing over to supervisord"

# From here fatalwatch.py (see supervisord.conf) owns the failure path: any program that exhausts
# its retries and enters FATAL brings supervisord — and therefore this container — down, so the
# platform restarts it instead of routing traffic to a half-dead image.
set +e
wait "${SUPERVISORD_PID}"
supervisord_status=$?
set -e

# supervisord treats fatalwatch's SIGTERM as a graceful shutdown and exits 0. Left alone that would
# report a dead service to the platform as a clean stop, so the trip file overrides it.
if [ -f "${FATALWATCH_TRIP_FILE:-/run/fatalwatch.trip}" ]; then
  echo "nvda-platform: EXITING NON-ZERO: $(cat "${FATALWATCH_TRIP_FILE:-/run/fatalwatch.trip}")" >&2
  exit 1
fi
exit "${supervisord_status}"
