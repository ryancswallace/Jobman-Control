#!/bin/sh

set -eu

latest=''
for candidate in $(git tag --merged HEAD --list 'v*' --sort=-v:refname); do
  if printf '%s\n' "${candidate}" \
    | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
    latest=${candidate}
    break
  fi
done

if [ -z "${latest}" ]; then
  echo 'No stable release tag is reachable; release metadata is not yet required.'
  exit 0
fi

version=${latest#v}
heading=$(grep -E "^## \[${version}\] - [0-9]{4}-[0-9]{2}-[0-9]{2}$" CHANGELOG.md || true)
if [ "$(printf '%s\n' "${heading}" | sed '/^$/d' | wc -l | tr -d ' ')" -ne 1 ]; then
  echo "CHANGELOG.md must contain exactly one dated ${version} release heading" >&2
  exit 1
fi
release_date=${heading##* - }

if ! grep -Fxq "version: ${version}" CITATION.cff; then
  echo "CITATION.cff version does not match ${latest}" >&2
  exit 1
fi
if ! grep -Fxq "date-released: ${release_date}" CITATION.cff; then
  echo "CITATION.cff release date does not match CHANGELOG.md" >&2
  exit 1
fi
if ! grep -Fxq "[${version}]: https://github.com/ryancswallace/jobman-control/releases/tag/${latest}" CHANGELOG.md; then
  echo "CHANGELOG.md is missing the canonical ${latest} release link" >&2
  exit 1
fi

printf 'Verified release metadata for %s (%s).\n' "${latest}" "${release_date}"
