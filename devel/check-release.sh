#!/bin/sh

set -eu

dist=${1:-dist}
mode=${2:-snapshot}
if [ "${mode}" != snapshot ] && [ "${mode}" != signed ]; then
  echo "usage: $0 [DIST] [snapshot|signed]" >&2
  exit 2
fi

manifest=$(find "${dist}" -maxdepth 1 -type f -name 'jobman-control_*_checksums.txt' -print)
if [ -z "${manifest}" ] || [ "$(printf '%s\n' "${manifest}" | wc -l | tr -d ' ')" -ne 1 ]; then
  echo "expected exactly one checksum manifest in ${dist}" >&2
  exit 1
fi
manifest_name=$(basename "${manifest}")
version=${manifest_name#jobman-control_}
version=${version%_checksums.txt}
if [ -z "${version}" ]; then
  echo "could not determine release version from ${manifest_name}" >&2
  exit 1
fi

expected=''
for target in \
  darwin_amd64.tar.gz darwin_arm64.tar.gz \
  linux_386.tar.gz linux_amd64.tar.gz linux_arm64.tar.gz \
  windows_386.zip windows_amd64.zip windows_arm64.zip; do
  expected="${expected} jobman-control_${version}_${target}"
done
for architecture in 386 amd64 arm64; do
  for format in apk deb rpm; do
    expected="${expected} jobman-control_${version}_linux_${architecture}.${format}"
  done
done

for filename in ${expected}; do
  if [ ! -s "${dist}/${filename}" ]; then
    echo "release is missing ${filename}" >&2
    exit 1
  fi
  if [ "$(awk -v name="${filename}" '$2 == name { count++ } END { print count + 0 }' "${manifest}")" -ne 1 ]; then
    echo "checksum manifest must contain ${filename} exactly once" >&2
    exit 1
  fi
  if [ ! -s "${dist}/${filename}.sbom.json" ]; then
    echo "release is missing ${filename}.sbom.json" >&2
    exit 1
  fi
done

if [ "${mode}" = signed ] && [ ! -s "${manifest}.sigstore.json" ]; then
  echo "release is missing ${manifest_name}.sigstore.json" >&2
  exit 1
fi

(
  cd "${dist}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum --check "${manifest_name}"
  else
    shasum -a 256 -c "${manifest_name}"
  fi
)

for archive in \
  "${dist}/jobman-control_${version}_darwin_amd64.tar.gz" \
  "${dist}/jobman-control_${version}_darwin_arm64.tar.gz" \
  "${dist}/jobman-control_${version}_linux_386.tar.gz" \
  "${dist}/jobman-control_${version}_linux_amd64.tar.gz" \
  "${dist}/jobman-control_${version}_linux_arm64.tar.gz"; do
  tar -tzf "${archive}" | grep -Fxq jobman-control || {
    echo "$(basename "${archive}") does not contain jobman-control" >&2
    exit 1
  }
done
for archive in \
  "${dist}/jobman-control_${version}_windows_386.zip" \
  "${dist}/jobman-control_${version}_windows_amd64.zip" \
  "${dist}/jobman-control_${version}_windows_arm64.zip"; do
  unzip -Z1 "${archive}" | grep -Fxq jobman-control.exe || {
    echo "$(basename "${archive}") does not contain jobman-control.exe" >&2
    exit 1
  }
done

printf 'Verified Jobman Control release %s: 8 archives, 9 native packages, and 17 SBOMs.\n' "${version}"
