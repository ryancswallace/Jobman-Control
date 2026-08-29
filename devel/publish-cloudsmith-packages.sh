#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 1 || ! "$1" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "usage: $0 vMAJOR.MINOR.PATCH" >&2
  exit 2
fi
if [[ -z "${GH_TOKEN:-}" || -z "${CLOUDSMITH_API_KEY:-}" ]]; then
  echo 'GH_TOKEN and CLOUDSMITH_API_KEY are required' >&2
  exit 2
fi

release_tag=$1
version=${release_tag#v}
repository=jobman/stable
artifact_dir=$(mktemp -d)
trap 'rm -rf "${artifact_dir}"' EXIT

state=$(gh release view "${release_tag}" --repo ryancswallace/jobman-control \
  --json isDraft,isPrerelease,tagName --jq '[.isDraft,.isPrerelease,.tagName] | @tsv')
IFS=$'\t' read -r draft prerelease resolved_tag <<< "${state}"
if [[ "${draft}" != false || "${prerelease}" != false || "${resolved_tag}" != "${release_tag}" ]]; then
  echo "${release_tag} is not the exact published stable release" >&2
  exit 1
fi

gh release download "${release_tag}" --repo ryancswallace/jobman-control \
  --pattern "jobman-control_${version}_checksums.txt" \
  --pattern "jobman-control_${version}_checksums.txt.sigstore.json" \
  --pattern "jobman-control_${version}_linux_*.apk" \
  --pattern "jobman-control_${version}_linux_*.deb" \
  --pattern "jobman-control_${version}_linux_*.rpm" \
  --dir "${artifact_dir}"

manifest=${artifact_dir}/jobman-control_${version}_checksums.txt
cosign verify-blob \
  --bundle "${manifest}.sigstore.json" \
  --certificate-identity \
    "https://github.com/ryancswallace/jobman-control/.github/workflows/release.yml@refs/heads/main" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "${manifest}"

for architecture in 386 amd64 arm64; do
  for format in apk deb rpm; do
    filename=jobman-control_${version}_linux_${architecture}.${format}
    package=${artifact_dir}/${filename}
    if [[ ! -s "${package}" ]]; then
      echo "release is missing ${filename}" >&2
      exit 1
    fi
    (cd "${artifact_dir}" && sha256sum --check --ignore-missing "$(basename "${manifest}")") >/dev/null
    gh attestation verify "${package}" \
      --repo ryancswallace/jobman-control \
      --signer-workflow ryancswallace/jobman-control/.github/workflows/release.yml \
      --source-ref refs/heads/main >/dev/null
    digest=$(sha256sum "${package}" | awk '{print $1}')
    case ${format} in
      apk) target=${repository}/alpine/any-version; kind=alpine ;;
      deb) target=${repository}/any-distro/any-version; kind=deb ;;
      rpm) target=${repository}/any-distro/any-version; kind=rpm ;;
    esac
    existing=$(cloudsmith list packages "${repository}" --output-format json --query "filename:^${filename}$")
    if jq -e --arg filename "${filename}" --arg tag "source-sha256-${digest}" \
      'any(.data[]; .filename == $filename and ((.tags.info // []) | index($tag)))' \
      <<< "${existing}" >/dev/null; then
      echo "Cloudsmith already contains verified ${filename}."
      continue
    fi
    if jq -e --arg filename "${filename}" 'any(.data[]; .filename == $filename)' \
      <<< "${existing}" >/dev/null; then
      echo "Cloudsmith contains conflicting ${filename}" >&2
      exit 1
    fi
    cloudsmith push "${kind}" "${target}" "${package}" \
      --tags "stable,source-sha256-${digest}"
  done
done
