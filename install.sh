#!/bin/sh
# Install a published release without a Go toolchain.
set -eu

fail() {
    printf 'brokk-feature-bot installer: %s\n' "$*" >&2
    exit 1
}

download() {
    curl --fail --silent --show-error --location --retry 3 \
        --proto '=https' --proto-redir '=https' "$@"
}

main() {
    if [ "$#" -gt 1 ]; then
        fail 'usage: sh install.sh [VERSION]; set INSTALL_DIR to change the destination'
    fi
    case "${1:-}" in
        -h|--help)
            printf 'Usage: sh install.sh [VERSION]\nDefaults: latest stable release, INSTALL_DIR=$HOME/.local/bin\n'
            return
            ;;
    esac

    for tool in curl tar awk mktemp uname; do
        command -v "$tool" >/dev/null 2>&1 || fail "required command not found: $tool"
    done
    if command -v sha256sum >/dev/null 2>&1; then
        checksum_tool=sha256sum
    elif command -v shasum >/dev/null 2>&1; then
        checksum_tool=shasum
    else
        fail 'install sha256sum or shasum to verify downloads'
    fi

    case "$(uname -s)" in
        Linux) os=linux ;;
        Darwin) os=darwin ;;
        *) fail 'supported operating systems: Linux and macOS' ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        arm64|aarch64) arch=arm64 ;;
        *) fail 'supported architectures: amd64 and arm64' ;;
    esac

    releases=https://github.com/BrokkAi/feature-bot/releases
    version=${1:-latest}
    if [ "$version" = latest ]; then
        latest_url=$(download --output /dev/null --write-out '%{url_effective}' "$releases/latest") ||
            fail 'could not find the latest stable release; check that a release has been published'
        case "$latest_url" in
            "$releases/tag/"*) version=${latest_url##*/} ;;
            *) fail 'latest release did not resolve to a release tag' ;;
        esac
    fi
    printf '%s\n' "$version" | awk '
        /^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$/ { valid++ }
        END { exit !(NR == 1 && valid == 1) }
    ' || fail 'version must be a tag such as v0.1.0 or v0.1.0-rc.1'

    install_dir=${INSTALL_DIR:-${HOME:?set HOME or INSTALL_DIR}/.local/bin}
    case "$install_dir" in
        /*) ;;
        *) install_dir=$PWD/$install_dir ;;
    esac
    [ ! -d "$install_dir/bfb" ] || fail 'installation target is a directory'
    temporary=$(mktemp -d)
    staged=
    trap 'rm -rf "$temporary"; if [ -n "$staged" ]; then rm -f "$staged"; fi' 0
    trap 'exit 1' HUP INT TERM

    asset=brokk-feature-bot-$version-$os-$arch.tar.gz
    printf 'Downloading brokk-feature-bot %s for %s/%s...\n' "$version" "$os" "$arch"
    download --output "$temporary/$asset" "$releases/download/$version/$asset" || fail "could not download $asset"
    download --output "$temporary/checksums.txt" "$releases/download/$version/checksums.txt" || fail 'could not download checksums.txt'

    expected=$(awk -v asset="$asset" '$2 == asset { count++; hash = $1 } END { if (count != 1) exit 1; print hash }' "$temporary/checksums.txt") ||
        fail 'checksum must contain exactly one entry for the archive'
    if [ "$checksum_tool" = sha256sum ]; then
        actual=$(sha256sum "$temporary/$asset")
    else
        actual=$(shasum -a 256 "$temporary/$asset")
    fi
    actual=${actual%% *}
    [ "$actual" = "$expected" ] || fail "checksum mismatch for $asset"

    mkdir -p "$install_dir"
    staged=$(mktemp "$install_dir/.brokk-feature-bot.XXXXXX")
    # Extract only the binary to a fresh file; never follow archive paths or links.
    tar -xzOf "$temporary/$asset" bfb > "$staged" || fail 'could not extract bfb'
    [ -s "$staged" ] || fail 'archive contains an empty binary'
    chmod 755 "$staged"
    mv -f "$staged" "$install_dir/bfb"
    staged=
    printf 'Installed brokk-feature-bot %s to %s/bfb\n' "$version" "$install_dir"
    case ":${PATH:-}:" in
        *":$install_dir:"*) ;;
        *) printf 'Add %s to your PATH, then run bfb --help.\n' "$install_dir" ;;
    esac
}

main "$@"
