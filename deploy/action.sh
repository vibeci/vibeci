#!/usr/bin/env bash
# The GitHub Action (action.yml): builds the vibeci binary and the sandbox
# images from the action's own checkout of VibeCI, then runs
# `vibeci action`, which reads the inputs from VIBECI_ACTION_* (see
# `vibeci action -h`). Needs a Linux runner with Docker.
set -euo pipefail

root="${GITHUB_ACTION_PATH:?GITHUB_ACTION_PATH is not set: this script is run by action.yml}"
work="${RUNNER_TEMP:?RUNNER_TEMP is not set}/vibeci"

if [ "$(uname -s)" != Linux ]; then
  echo "::error title=VibeCI::needs a Linux runner with Docker (runs-on: ubuntu-latest)"
  exit 1
fi
if ! docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
  echo "::error title=VibeCI::needs a Docker engine on the runner"
  exit 1
fi
mkdir -p "$work/bin"
chmod 700 "$work"

echo "::group::Build VibeCI"
# The build stage of the release image: the same Go toolchain and flags.
docker build --quiet --target build --build-arg VERSION="${VIBECI_VERSION:-dev}" \
  --tag vibeci-action-build --file "$root/deploy/Dockerfile" "$root" >/dev/null
cid="$(docker create vibeci-action-build)"
docker cp --quiet "$cid:/out/vibeci" "$work/bin/vibeci"
docker rm "$cid" >/dev/null
"$work/bin/vibeci" version
echo "::endgroup::"

echo "::group::Build the sandbox images"
for image in ${VIBECI_ACTION_IMAGES:-base go}; do
  case "$image" in
    base | go) ;;
    *) echo "::error title=VibeCI::unknown sandbox image \"$image\" (images: base, go)"; exit 1 ;;
  esac
  docker build --quiet --tag "vibeci-sandbox:$image" --file "$root/deploy/sandbox/$image.Dockerfile" "$root/deploy/sandbox"
done
# Egress for prefetch profiles only, without traffic between sandboxes.
docker network inspect vibeci-egress >/dev/null 2>&1 ||
  docker network create -o com.docker.network.bridge.enable_icc=false vibeci-egress >/dev/null
echo "::endgroup::"

exec "$work/bin/vibeci" action
