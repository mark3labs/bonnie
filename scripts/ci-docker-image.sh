#!/usr/bin/env bash
# Prepare the Docker test image once, before parallel tests create containers.
# Keep full pull errors in CI logs. Do not skip tests when a registry fails.
set -euo pipefail

image=alpine:3.19
mirror=mirror.gcr.io/library/$image
for source in "$image" "$mirror"; do
  for attempt in 1 2 3; do
    if docker pull "$source"; then
      if [ "$source" != "$image" ]; then
        docker tag "$source" "$image"
      fi
      docker image inspect "$image" > /dev/null
      exit 0
    fi
    if [ "$attempt" -lt 3 ]; then
      sleep 5
    fi
  done
done

echo "Cannot prepare Docker test image $image from Docker Hub or its public mirror" >&2
exit 1
