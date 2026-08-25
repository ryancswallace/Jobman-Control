#!/bin/sh

set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 IMAGE VERSION COMMIT" >&2
  exit 2
fi

image=$1
version=$2
commit=$3
docker_command=${DOCKER:-docker}

version_output=$(${docker_command} run --rm "${image}" --version)
case ${version_output} in
  "jobman-control ${version}"*) ;;
  *)
    echo "container reported unexpected version: ${version_output}" >&2
    exit 1
    ;;
esac

uid=$(${docker_command} run --rm --entrypoint id "${image}" -u)
if [ "${uid}" = 0 ]; then
  echo 'runtime container must not run as root' >&2
  exit 1
fi

${docker_command} run --rm --entrypoint sh "${image}" -eu -c \
  'test -x /usr/local/bin/jobman-control && test -s /usr/share/licenses/jobman-control/LICENSE && test -s /usr/share/licenses/jobman-control/THIRD_PARTY_NOTICES.md'

revision=$(${docker_command} image inspect "${image}" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
if [ "${revision}" != "${commit}" ]; then
  echo "container revision ${revision} does not match ${commit}" >&2
  exit 1
fi

printf 'Verified non-root Jobman Control container %s.\n' "${version}"
