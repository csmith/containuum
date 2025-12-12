#!/bin/bash
set -euo pipefail

LABEL="containuum.test=true"

cleanup() {
    docker ps -aq --filter "label=$LABEL" | xargs -r docker rm -f >/dev/null 2>&1 || true
}
trap cleanup EXIT

cleanup

echo "Creating container test1..."
docker run -d --label "$LABEL" --name containuum-test1 alpine:latest sleep 30 >/dev/null

sleep 0.5

echo "Creating container test2..."
docker run -d --label "$LABEL" --name containuum-test2 alpine:latest sleep 30 >/dev/null

sleep 0.5

echo "Stopping test1..."
docker stop containuum-test1 >/dev/null

sleep 0.5

echo "Removing test2..."
docker rm -f containuum-test2 >/dev/null

sleep 0.5

echo "Done"
