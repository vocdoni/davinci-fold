#!/usr/bin/env bash
# Live end-to-end run against real davinci-zkvm provers, everything in Docker.
# The orchestrator runs as the image built from this tree; TestFailoverE2E
# drives it from a golang container: three elections, a SIGKILL of the
# orchestrator while batches are proved, a fold worker removed and registered
# again (see docs/testing.md).
#
# Usage:
#   DAVINCI_FOLD_WORKER_URLS=http://<prover-a>:8080,http://<prover-b>:8080 scripts/e2e-live.sh
#
# Env:
#   DAVINCI_FOLD_WORKER_URLS  prover base URLs, comma-separated (required, two or more)
#   E2E_ZKVM_DIR              davinci-zkvm checkout       (default ../davinci-zkvm)
#   E2E_PORT                  orchestrator API port       (default 28888, on 127.0.0.1)
#   E2E_LOG_DIR               logs, one directory per run (default ~/.cache/davinci-fold-e2e)
#   E2E_TIMEOUT               go test timeout             (default 90m)
#
# The image is davinci-fold:e2e; containers and volumes are named
# davinci-fold-e2e-*. The containers and the data volume are removed when the
# run ends; the Go cache volumes are kept for the next run.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
zkvm=$(cd "${E2E_ZKVM_DIR:-$root/../davinci-zkvm}" && pwd -P)
port=${E2E_PORT:-28888}
timeout=${E2E_TIMEOUT:-90m}
logs=${E2E_LOG_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/davinci-fold-e2e}/$(date -u +%Y%m%dT%H%M%SZ)
workers=${DAVINCI_FOLD_WORKER_URLS:?set DAVINCI_FOLD_WORKER_URLS to two or more prover URLs}

image=davinci-fold:e2e
orchestrator=davinci-fold-e2e-orchestrator
driver=davinci-fold-e2e-test
solc=davinci-fold-e2e-solc
data=davinci-fold-e2e-data
jwt_secret=davinci-fold-integration-secret # tests/helpers.TestJWTSecret

for c in "$orchestrator" "$driver" "$solc"; do
  if docker container inspect "$c" >/dev/null 2>&1; then
    echo "container $c exists: another run is in progress, or remove it with: docker rm -f $c" >&2
    exit 1
  fi
done
if docker volume inspect "$data" >/dev/null 2>&1; then
  echo "volume $data exists: another run is in progress, or remove it with: docker volume rm $data" >&2
  exit 1
fi
IFS=, read -ra urls <<<"$workers"
for u in "${urls[@]}"; do
  curl -sf -m 10 "$u/health" >/dev/null || { echo "prover $u is not healthy" >&2; exit 1; }
done
if curl -s -m 2 "http://127.0.0.1:$port/ping" >/dev/null 2>&1; then
  echo "port $port is in use, set E2E_PORT" >&2
  exit 1
fi

mkdir -p "$logs"
cleanup() {
  docker logs "$orchestrator" >"$logs/orchestrator.log" 2>&1 || true
  docker rm -f "$orchestrator" "$driver" "$solc" >/dev/null 2>&1 || true
  docker volume rm "$data" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "logs: $logs"
docker build -t "$image" "$root" >"$logs/build.log" 2>&1 || { echo "image build failed, see $logs/build.log" >&2; exit 1; }

docker volume create "$data" >/dev/null
docker run -d --name "$orchestrator" --network host \
  -v "$data":/app/run \
  -e DAVINCIFOLD_DATADIR=/app/run \
  -e DAVINCIFOLD_API_HOST=127.0.0.1 \
  -e DAVINCIFOLD_API_PORT="$port" \
  -e DAVINCIFOLD_API_JWTSECRET="$jwt_secret" \
  -e DAVINCIFOLD_LOG_LEVEL=debug \
  "$image" >/dev/null

# The driver kills and restarts the orchestrator through the Docker socket
# and reads its data volume once it stopped it.
docker create --name "$driver" --network host --memory 16g \
  -v "$root":/src/davinci-fold:ro \
  -v "$zkvm":/src/davinci-zkvm:ro \
  -v davinci-fold-e2e-gomod:/go/pkg/mod \
  -v davinci-fold-e2e-gocache:/root/.cache/go-build \
  -v "$data":/fold-data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -w /src/davinci-fold \
  -e RUN_INTEGRATION_TESTS=1 \
  -e DAVINCI_FOLD_URL="http://127.0.0.1:$port" \
  -e DAVINCI_FOLD_CONTAINER="$orchestrator" \
  -e DAVINCI_FOLD_DATADIR=/fold-data \
  -e DAVINCI_FOLD_WORKER_URLS="$workers" \
  golang:1.25 \
  go test ./tests/ -run '^TestFailoverE2E$' -count=1 -v -timeout "$timeout" >/dev/null

# The final PLONK is verified with solc, taken from the image the Go SDK pins.
docker create --name "$solc" ethereum/solc:0.8.28 >/dev/null
docker cp -L "$solc:/usr/bin/solc" - | docker cp - "$driver:/usr/local/bin"

docker start -a "$driver" 2>&1 | tee "$logs/test.log" || true
status=$(docker inspect -f '{{.State.ExitCode}}' "$driver")
if [ "$status" = 0 ]; then
  echo "E2E PASS (logs: $logs)"
else
  echo "E2E FAIL (logs: $logs)" >&2
fi
exit "$status"
