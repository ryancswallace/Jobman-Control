#!/bin/sh

set -eu

if [ -z "${GO_VERS:-}" ]; then
  echo 'error: GO_VERS is undefined' >&2
  exit 166
fi
if ! printf '%s\n' "${GO_VERS}" | grep -Eq '^[0-9]+\.[0-9]+(\.[0-9]+)?((rc|beta)[0-9]+)?$'; then
  echo "error: unsupported Go version: ${GO_VERS}" >&2
  exit 167
fi

temporary=''
trap 'rm -f "${temporary:-}"' EXIT HUP INT TERM

replace() {
  expression=$1
  shift
  for file do
    temporary=$(mktemp "${file}.tmp.XXXXXXXXXX")
    sed "${expression}" "${file}" > "${temporary}"
    cat "${temporary}" > "${file}"
    rm -f "${temporary}"
    temporary=''
  done
}

language_version=$(printf '%s\n' "${GO_VERS}" | sed -E 's/^([0-9]+\.[0-9]+).*/\1/')
printf '%s\n' "${GO_VERS}" > go.version
replace "s/^go .*/go ${language_version}/" go.mod
replace "s/^  go: \".*\"/  go: \"${language_version}\"/" .golangci.yml
replace "s/^ARG GO_VERSION=.*/ARG GO_VERSION=${GO_VERS}/" Dockerfile .devcontainer/Dockerfile
replace "s/^ARG GO_FEATURE_VERSION=.*/ARG GO_FEATURE_VERSION=${language_version}/" .devcontainer/Dockerfile
replace "s/\"GO_VERSION\": \"[^\"]*\"/\"GO_VERSION\": \"${GO_VERS}\"/" .devcontainer/devcontainer.json
replace "s/Go [0-9][0-9.]*/Go ${GO_VERS}/g" .devcontainer/README.md

trap - EXIT HUP INT TERM
