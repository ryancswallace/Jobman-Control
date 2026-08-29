#!/bin/sh

set -eu

if [ "$#" -gt 1 ]; then
  echo 'usage: check-release-metadata.sh [vMAJOR.MINOR.PATCH]' >&2
  exit 2
fi

candidate=${RELEASE_TAG:-}
if [ "$#" -eq 1 ]; then
  if [ -n "${candidate}" ] && [ "${candidate}" != "$1" ]; then
    echo 'release tag argument conflicts with RELEASE_TAG' >&2
    exit 2
  fi
  candidate=$1
fi

if [ -n "${candidate}" ] && ! printf '%s\n' "${candidate}" \
  | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
  echo "release metadata tag must be a stable semantic version: ${candidate}" >&2
  exit 2
fi

stable_tags=$(git tag --merged HEAD --list 'v*' --sort=v:refname \
  | grep -E '^v([1-9][0-9]*\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)|0\.[1-9][0-9]*\.(0|[1-9][0-9]*))$' \
  || true)
if [ -n "${candidate}" ] && ! printf '%s\n' "${stable_tags}" \
  | grep -Fxq "${candidate}"; then
  stable_tags=$(printf '%s\n%s\n' "${stable_tags}" "${candidate}" | sed '/^$/d')
fi

if grep -Eq '^(version|date-released):' CITATION.cff; then
  echo 'CITATION.cff must describe the project, not an individual release' >&2
  exit 1
fi

if [ -z "${stable_tags}" ]; then
  echo 'No stable release tag is reachable; release metadata is not yet required.'
  exit 0
fi

previous=
latest=
for release_tag in ${stable_tags}; do
  version=${release_tag#v}
  heading=$(grep -E "^## \[${version}\] - [0-9]{4}-[0-9]{2}-[0-9]{2}$" CHANGELOG.md || true)
  if [ "$(printf '%s\n' "${heading}" | sed '/^$/d' | wc -l | tr -d ' ')" -ne 1 ]; then
    echo "CHANGELOG.md must contain exactly one dated ${version} release heading" >&2
    exit 1
  fi

  if git rev-parse --verify --quiet "refs/tags/${release_tag}" >/dev/null; then
    release_date=$(git show -s --format=%cs "${release_tag}^{commit}")
    if [ "${heading##* - }" != "${release_date}" ]; then
      echo "CHANGELOG.md release date for ${release_tag} does not match its commit" >&2
      exit 1
    fi
  fi

  if [ -n "${previous}" ]; then
    release_url="https://github.com/ryancswallace/jobman-control/compare/${previous}...${release_tag}"
  else
    release_url="https://github.com/ryancswallace/jobman-control/releases/tag/${release_tag}"
  fi
  if ! grep -Fxq "[${version}]: ${release_url}" CHANGELOG.md; then
    echo "CHANGELOG.md is missing the canonical ${release_tag} release link" >&2
    exit 1
  fi
  previous=${release_tag}
  latest=${release_tag}
done

if ! grep -Fxq "[Unreleased]: https://github.com/ryancswallace/jobman-control/compare/${latest}...HEAD" CHANGELOG.md; then
  echo "CHANGELOG.md has a stale Unreleased comparison link" >&2
  exit 1
fi

printf 'Verified release metadata through %s.\n' "${latest}"
