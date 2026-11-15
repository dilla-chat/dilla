#!/usr/bin/env bash
set -euo pipefail

# Only these reviewed release assets may supply executable CI tools. The hashes
# are the SHA-256 digests of the archives, not of publisher-supplied hash files.
tool=${1:?usage: install-ci-tool.sh TOOL [--archive FILE]}
case "$tool" in
  wasm-pack)
    version=0.15.0
    archive_name=wasm-pack-v0.15.0-x86_64-unknown-linux-musl.tar.gz
    url=https://github.com/wasm-bindgen/wasm-pack/releases/download/v0.15.0/$archive_name
    sha256=c09f971ecaed9a2efc80fdcea7a00ef6b53c7fadc8c57d1f61b53a6aa66b668a
    prefix=wasm-pack-v0.15.0-x86_64-unknown-linux-musl
    binaries=(wasm-pack)
    ;;
  wasm-bindgen)
    version=0.2.128
    archive_name=wasm-bindgen-0.2.128-x86_64-unknown-linux-musl.tar.gz
    url=https://github.com/wasm-bindgen/wasm-bindgen/releases/download/0.2.128/$archive_name
    sha256=b51f0208fdff83515a787bd8ab9ac5865ed84dabb66d0c709957bb59793c645f
    prefix=wasm-bindgen-0.2.128-x86_64-unknown-linux-musl
    binaries=(wasm-bindgen wasm-bindgen-test-runner wasm2es6js)
    ;;
  binaryen)
    version=133
    archive_name=binaryen-version_133-x86_64-linux.tar.gz
    url=https://github.com/WebAssembly/binaryen/releases/download/version_133/$archive_name
    sha256=2dc9c7813f5375db93d96ead4b78222fcc3e2677bbb832297af4797782a37489
    prefix=binaryen-version_133
    binaries=(bin/wasm-opt)
    ;;
  cargo-deny)
    version=0.20.2
    archive_name=cargo-deny-0.20.2-x86_64-unknown-linux-musl.tar.gz
    url=https://github.com/EmbarkStudios/cargo-deny/releases/download/0.20.2/$archive_name
    sha256=9f12ed4c49936e09b48bf862b595cde2fe64fcbd9d74dfacac6131ca824c8d5f
    prefix=cargo-deny-0.20.2-x86_64-unknown-linux-musl
    binaries=(cargo-deny)
    ;;
  golangci-lint)
    version=2.14.0
    archive_name=golangci-lint-2.14.0-linux-amd64.tar.gz
    url=https://github.com/golangci/golangci-lint/releases/download/v2.14.0/$archive_name
    sha256=ab90aeb7b066f92a33415b638a50fe5344bbb75a0d32ad30cc248d88f81032ab
    prefix=golangci-lint-2.14.0-linux-amd64
    binaries=(golangci-lint)
    ;;
  sqlc)
    version=1.31.1
    archive_name=sqlc_1.31.1_linux_amd64.tar.gz
    url=https://github.com/sqlc-dev/sqlc/releases/download/v1.31.1/$archive_name
    sha256=497ae4fcdfa64c5b0c311ffe4c2bd991e43991e82e5367792ed78bc2dca27354
    prefix=
    binaries=(sqlc)
    ;;
  # The GPUI desktop release (desktop-gpui-release.yml). appimagetool is itself an AppImage (a single file, run with
  # APPIMAGE_EXTRACT_AND_RUN=1 where there is no FUSE). Its 1.9.1 release asset reports "continuous build (git version
  # 8c8c91f)" but is the dated 1.9.1 release, never the moving `continuous` tag. appimagetool would fetch the AppImage
  # runtime from type2-runtime's `continuous` release at build time; appimage-runtime pins the dated one instead, and
  # the package script hands it over with --runtime-file.
  appimagetool)
    version=1.9.1
    archive_name=appimagetool-x86_64.AppImage
    url=https://github.com/AppImage/appimagetool/releases/download/1.9.1/$archive_name
    sha256=ed4ce84f0d9caff66f50bcca6ff6f35aae54ce8135408b3fa33abfc3cb384eb0
    single=appimagetool
    ;;
  appimage-runtime)
    version=20251108
    archive_name=runtime-x86_64
    url=https://github.com/AppImage/type2-runtime/releases/download/20251108/$archive_name
    sha256=2fca8b443c92510f1483a883f60061ad09b46b978b2631c807cd873a47ec260d
    single=appimage-runtime-x86_64
    ;;
  nfpm)
    version=2.48.0
    archive_name=nfpm_2.48.0_Linux_x86_64.tar.gz
    url=https://github.com/goreleaser/nfpm/releases/download/v2.48.0/$archive_name
    sha256=6d7f9a28b2f397934403950fcbba83f598b08d02edbe79a0815b3b4897341750
    prefix=
    binaries=(nfpm)
    ;;
  *) echo "unapproved CI tool: $tool" >&2; exit 1 ;;
esac

if [[ $# -ne 1 && ! ( $# -eq 3 && $2 == --archive ) ]]; then
  echo 'usage: install-ci-tool.sh TOOL [--archive FILE]' >&2
  exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
archive=$tmp/$archive_name
if [[ $# -eq 3 ]]; then
  cp -- "$3" "$archive"
else
  curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location --retry 3 "$url" -o "$archive"
fi
printf '%s  %s\n' "$sha256" "$archive" | sha256sum --check --status || {
  echo "SHA-256 mismatch for $tool $version" >&2
  exit 1
}

bin_dir=${RUNNER_TEMP:-${TMPDIR:-/tmp}}/dilla-ci-tools
mkdir -p "$bin_dir"
if [[ -n ${single:-} ]]; then
  install -m 0755 "$archive" "$bin_dir/$single"
else
  for binary in "${binaries[@]}"; do
    name=${binary##*/}
    tar -xOf "$archive" "${prefix:+$prefix/}$binary" > "$tmp/$name"
    install -m 0755 "$tmp/$name" "$bin_dir/$name"
  done
fi
if [[ -n ${GITHUB_PATH:-} ]]; then
  printf '%s\n' "$bin_dir" >> "$GITHUB_PATH"
fi

case "$tool" in
  wasm-pack) "$bin_dir/wasm-pack" --version | grep -Fx "wasm-pack $version" ;;
  wasm-bindgen) "$bin_dir/wasm-bindgen" --version | grep -Fx "wasm-bindgen $version" ;;
  binaryen) "$bin_dir/wasm-opt" --version | grep -Fx "wasm-opt version $version (version_$version)" ;;
  cargo-deny) "$bin_dir/cargo-deny" --version | grep -Fx "cargo-deny $version" ;;
  golangci-lint) "$bin_dir/golangci-lint" version | grep -F "version $version" ;;
  sqlc) "$bin_dir/sqlc" version | grep -Fx "v$version" ;;
  appimagetool) APPIMAGE_EXTRACT_AND_RUN=1 "$bin_dir/appimagetool" --version 2>&1 | grep -F "(git version 8c8c91f)" ;;
  appimage-runtime) ;;  # not a program: the digest above is its whole check
  nfpm) "$bin_dir/nfpm" --version | grep -Fx "GitVersion:    $version" ;;
esac
