#!/usr/bin/env bash
# Benchmarks local vs. distributed compiles on a Docker cluster.
#
# Usage: test/bench.sh [bench.py args, e.g. --files 100 --cflags -O3]
#   WORKERS=10       worker containers
#   WORKER_JOBS=1    concurrent compiles per worker
#   BENCH_CPUS=0.8   CPU cap for the client container (models a thin client
#                    offloading to a farm; all containers share one Docker host)

set -euo pipefail
cd "$(dirname "$0")/.."
WORKERS="${WORKERS:-10}"
WORKER_JOBS="${WORKER_JOBS:-1}"
BENCH_CPUS="${BENCH_CPUS:-0.8}"
LABEL=distbuild-bench
NET=$LABEL-net

cleanup() {
    docker ps -aq --filter "label=$LABEL" | xargs -r docker rm -f >/dev/null
    docker network rm "$NET" >/dev/null 2>&1 || true
}

echo "Building images..."
tar -c Dockerfile go.mod cmd | docker build -q -t distbuild:latest - >/dev/null
docker build -q -t distbuild-bench:latest - < test/Dockerfile.bench >/dev/null

cleanup
trap cleanup EXIT
docker network create "$NET" >/dev/null

echo "Starting $WORKERS workers and orchestrator..."
# Workers share the alias "worker", so the orchestrator resolves it to every worker IP.
for i in $(seq "$WORKERS"); do
    docker run -d --label $LABEL --network "$NET" --network-alias worker \
        -e WORKER_JOBS="$WORKER_JOBS" distbuild:latest worker >/dev/null
done
docker run -d --label $LABEL --name $LABEL-orchestrator --network "$NET" \
    -e WORKER_SERVICE=worker -e WORKER_PORT=8080 distbuild:latest orchestrator >/dev/null

# The Linux clone lives in a named volume so it is reused across runs.
docker run --rm --label $LABEL --network "$NET" --cpus "$BENCH_CPUS" \
    -v "$PWD/test:/work/test:ro" -v $LABEL-linux-src:/src \
    distbuild-bench:latest \
    python3 /work/test/bench.py --orchestrator http://$LABEL-orchestrator:8080 "$@"
