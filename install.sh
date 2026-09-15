#!/bin/sh
# codectx installer.
#
# Downloads one released archive, verifies its SHA-256 against the release's
# checksums.txt *before* unpacking anything, and copies the binary into a
# prefix. Every asset it fetches is named by a line of that checksums.txt; it
# constructs no other asset URL. Nothing is written to the prefix until the
# digest matches.
#
# POSIX sh only: no bashisms, no `local`, no arrays, no [[ ]].
#
#   curl -fsSL <release>/latest/download/install.sh | sh
#   curl -fsSL <release>/latest/download/install.sh | sh -s -- --version 1.2.3
#
# Options:
#   --version <X.Y.Z|latest>  release to install (default: latest)
#   --prefix <dir>            install directory (default: $HOME/.local/bin)
#   --bundle                  install the offline bundle (binary + tool store)
#   --no-tools                install only the binary; do not install the toolchain
#   --tools-for-repo <dir>    install only the tools that repository needs
#   --dry-run                 print what would be done; write nothing, fetch nothing
#   -h, --help                print this help
#
# Environment:
#   CODECTX_INSTALL_BASE_URL  releases base URL, for mirrors and testing
#                             (default: the project's GitHub releases page)

set -eu

ctx_releases_url="${CODECTX_INSTALL_BASE_URL:-https://github.com/Sawmonabo/codectx/releases}"
ctx_version=latest
ctx_prefix="${HOME:-}/.local/bin"
ctx_bundle=no
ctx_no_tools=no
ctx_tools_for_repo=
ctx_dry_run=no
ctx_tmp=

ctx_log() { printf '%s\n' "codectx: $1" >&2; }

ctx_die() {
	printf '%s\n' "codectx: $1" >&2
	exit 1
}

# ctx_store_dir echoes the machine-wide tool store, resolved by exactly the rule
# the product uses (internal/config.osUserDataDir): XDG_DATA_HOME is honoured
# only when it is absolute, because a relative one names a store that moves with
# the process working directory, which the product ignores and its configuration
# validation rejects. If the installer resolved it differently, a bundle would
# land in a directory nothing reads.
ctx_store_dir() {
	case "${XDG_DATA_HOME:-}" in
	/*) printf '%s\n' "$XDG_DATA_HOME/codectx/tools" ;;
	*) printf '%s\n' "${HOME:-}/.local/share/codectx/tools" ;;
	esac
}

# ctx_usage is inline text, not a slice of this file: the script is normally
# run by piping it into sh, where $0 is not a readable path.
ctx_usage() {
	cat <<'EOF'
codectx installer.

Downloads one released archive, verifies its SHA-256 against the release's
checksums.txt before unpacking anything, and copies the binary into a prefix.

Usage:
  curl -fsSL <releases>/latest/download/install.sh | sh
  curl -fsSL <releases>/latest/download/install.sh | sh -s -- --version 1.2.3

Options:
  --version <X.Y.Z|latest>  release to install (default: latest)
  --prefix <dir>            install directory (default: $HOME/.local/bin)
  --bundle                  install the offline bundle (binary + tool store)
  --no-tools                install only the binary; do not install the toolchain
  --tools-for-repo <dir>    install only the tools that repository needs, instead
                            of every pinned tool
  --dry-run                 print what would be done; write nothing, fetch nothing
  -h, --help                print this help

After the binary is installed and verified, the installer runs
"codectx tools prefetch" so every pinned analyzer, language server and runtime
is present before the first index. They land in one machine-wide store, shared
by every repository on this host:
EOF
	printf '\n  %s\n\n' "$(ctx_store_dir)"
	cat <<'EOF'
The whole toolchain is several gigabytes; --tools-for-repo installs the subset one
repository selects, and --no-tools skips it entirely (a later run installs each
tool on demand).

Environment:
  CODECTX_INSTALL_BASE_URL  releases base URL, for mirrors and testing
EOF
}

ctx_cleanup() {
	if [ -n "$ctx_tmp" ] && [ -d "$ctx_tmp" ]; then
		rm -rf "$ctx_tmp"
	fi
}

# ctx_parse_args reads the documented options. An unknown option is a refusal,
# never a silently ignored word: a typo must not turn a pinned install into a
# latest install.
ctx_parse_args() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--version)
			[ "$#" -ge 2 ] || ctx_die "--version needs a value"
			ctx_version="$2"
			shift 2
			;;
		--version=*)
			ctx_version="${1#--version=}"
			shift
			;;
		--prefix)
			[ "$#" -ge 2 ] || ctx_die "--prefix needs a value"
			ctx_prefix="$2"
			shift 2
			;;
		--prefix=*)
			ctx_prefix="${1#--prefix=}"
			shift
			;;
		--bundle)
			ctx_bundle=yes
			shift
			;;
		--no-tools)
			ctx_no_tools=yes
			shift
			;;
		--tools-for-repo)
			[ "$#" -ge 2 ] || ctx_die "--tools-for-repo needs a value"
			ctx_tools_for_repo="$2"
			shift 2
			;;
		--tools-for-repo=*)
			ctx_tools_for_repo="${1#--tools-for-repo=}"
			shift
			;;
		--dry-run)
			ctx_dry_run=yes
			shift
			;;
		-h | --help)
			ctx_usage
			exit 0
			;;
		*)
			ctx_die "unknown option $1 (try --help)"
			;;
		esac
	done
	[ -n "$ctx_prefix" ] || ctx_die "--prefix must not be empty"
	if [ "$ctx_no_tools" = yes ] && [ -n "$ctx_tools_for_repo" ]; then
		ctx_die "--no-tools skips the toolchain, so it cannot be combined with --tools-for-repo"
	fi
}

# ctx_require_tools fails closed when a tool the verified path needs is absent,
# rather than degrading to an unverified install.
ctx_require_tools() {
	command -v curl >/dev/null 2>&1 ||
		ctx_die "curl is required to download the release"
	command -v tar >/dev/null 2>&1 ||
		ctx_die "tar is required to unpack the release archive"
	if command -v sha256sum >/dev/null 2>&1; then
		ctx_sha_tool=sha256sum
	elif command -v shasum >/dev/null 2>&1; then
		ctx_sha_tool=shasum
	else
		ctx_die "neither sha256sum nor shasum is available; cannot verify the download"
	fi
}

ctx_sha256() {
	case "$ctx_sha_tool" in
	sha256sum) sha256sum "$1" | awk '{print $1}' ;;
	shasum) shasum -a 256 "$1" | awk '{print $1}' ;;
	esac
}

# ctx_detect_platform maps uname to one §24 asset platform. WSL reports Linux
# and uses the linux build, so it is not a third branch.
ctx_detect_platform() {
	ctx_uname_s="$(uname -s)"
	ctx_uname_m="$(uname -m)"
	case "$ctx_uname_s" in
	Linux) ctx_os=linux ;;
	Darwin) ctx_os=darwin ;;
	*) ctx_die "unsupported operating system \"$ctx_uname_s\"; this installer supports Linux (including WSL) and macOS. On Windows install the .zip release manually." ;;
	esac
	case "$ctx_uname_m" in
	x86_64 | amd64) ctx_arch=amd64 ;;
	aarch64 | arm64) ctx_arch=arm64 ;;
	*) ctx_die "unsupported architecture \"$ctx_uname_m\"; this installer supports x86_64/amd64 and aarch64/arm64" ;;
	esac
	if [ "$ctx_os" = linux ]; then
		case "$(uname -r)" in
		*icrosoft* | *WSL*) ctx_log "WSL detected; installing the linux/$ctx_arch build" ;;
		esac
	fi
}

# ctx_resolve_version turns "latest" into a concrete tag by following the
# releases redirect, and normalises a pinned value so "1.2.3" and "v1.2.3" name
# the same release. The resolved version goes into asset names, so anything
# that is not a plain version token is refused before a URL is built. A
# character-class check alone is not enough: ".", ".." and a leading "-" are
# all spelled with allowed characters, and "v../checksums.txt" would be
# path-normalised by curl into a different release's asset. A version token
# therefore has to start with a digit.
ctx_resolve_version() {
	if [ "$ctx_version" = latest ]; then
		ctx_effective="$(curl -fsSL -o /dev/null -w '%{url_effective}' "$ctx_releases_url/latest")" ||
			ctx_die "could not resolve the latest release from $ctx_releases_url/latest"
		ctx_tag="${ctx_effective##*/}"
		[ -n "$ctx_tag" ] ||
			ctx_die "the latest-release redirect ($ctx_effective) does not name a tag; pin one with --version X.Y.Z"
		ctx_version="${ctx_tag#v}"
	else
		ctx_version="${ctx_version#v}"
		ctx_tag="v$ctx_version"
	fi
	case "$ctx_version" in
	'' | *[!A-Za-z0-9._-]* | [!0-9]*)
		ctx_die "\"$ctx_version\" is not a valid version; expected a form like 1.2.3"
		;;
	esac
}

# ctx_expected_digest reads the one checksums.txt line naming $1 and prints its
# hex digest. Zero lines or more than one line is a refusal: an asset the
# release does not name, or names twice, is never downloaded.
ctx_expected_digest() {
	# sha256sum/shasum binary mode prefixes the name with "*", and a checksum
	# file aggregated from a Windows leg can carry CRLF. Both are stripped; the
	# comparison stays exact, so a "dir/file" line still fails to match.
	ctx_matches="$(awk -v want="$1" '{ n = $2; sub(/^\*/, "", n); sub(/\r$/, "", n); if (n == want) print $1 }' "$ctx_tmp/checksums.txt")"
	ctx_count="$(printf '%s' "$ctx_matches" | grep -c . || true)"
	if [ "$ctx_count" -eq 0 ]; then
		ctx_die "release $ctx_tag does not publish $1 (it is not named in checksums.txt)"
	fi
	if [ "$ctx_count" -ne 1 ]; then
		ctx_die "checksums.txt names $1 $ctx_count times; refusing an ambiguous download"
	fi
	printf '%s\n' "$ctx_matches"
}

# ctx_download_verified fetches the asset named by checksums.txt and returns
# only if its SHA-256 matches. The asset URL may redirect to a content host;
# integrity comes from the digest, not from the hostname.
ctx_download_verified() {
	ctx_asset="$1"
	ctx_expected="$(ctx_expected_digest "$ctx_asset")"
	ctx_log "downloading $ctx_asset"
	curl -fsSL -o "$ctx_tmp/$ctx_asset" "$ctx_releases_url/download/$ctx_tag/$ctx_asset" ||
		ctx_die "could not download $ctx_asset from release $ctx_tag"
	ctx_actual="$(ctx_sha256 "$ctx_tmp/$ctx_asset")"
	if [ "$ctx_actual" != "$ctx_expected" ]; then
		ctx_die "SHA-256 mismatch for $ctx_asset
  expected $ctx_expected (checksums.txt)
  actual   $ctx_actual
Nothing was installed. Re-run, or report this release as corrupt."
	fi
	ctx_log "SHA-256 verified: $ctx_expected"
}

# ctx_install_tools installs the pinned toolchain with the binary that was just
# verified and copied, so the product works on its first index instead of
# fetching gigabytes in the middle of one. The payloads land in the shared
# machine-wide store the binary resolves by default, which is the same store the
# bundle path populates.
#
# The prefetch streams one line per tool as it lands; that output is the log the
# operator watches, so it is deliberately not captured or quieted.
#
# A failed prefetch does not un-install the binary and does not fail the script:
# the binary is installed and correct, every tool is still installed on demand
# by the first run that needs it, and turning a transient network fault into a
# non-zero install would be a worse answer than saying which step to re-run.
ctx_install_tools() {
	if [ "$ctx_no_tools" = yes ]; then
		ctx_log "skipping the toolchain (--no-tools); each tool installs on demand"
		return 0
	fi
	if [ "$ctx_bundle" = yes ]; then
		ctx_log "the bundle already carries every pinned tool; not fetching any"
		return 0
	fi
	if [ -n "$ctx_tools_for_repo" ]; then
		ctx_log "installing the pinned tools $ctx_tools_for_repo selects"
		set -- tools prefetch --for-repo "$ctx_tools_for_repo"
	else
		ctx_log "installing every pinned tool (several gigabytes; --no-tools skips it)"
		set -- tools prefetch --all
	fi
	if [ "$ctx_dry_run" = yes ]; then
		ctx_log "dry run: would run $ctx_prefix/codectx $*"
		return 0
	fi
	if "$ctx_prefix/codectx" "$@"; then
		ctx_log "the pinned toolchain is installed"
	else
		ctx_log "the toolchain did not finish installing. The binary is installed and every tool is still fetched on demand; to retry now, run: codectx tools prefetch --all"
	fi
}

ctx_main() {
	ctx_parse_args "$@"
	ctx_require_tools
	ctx_detect_platform

	# A dry run answers "what would this do here" without a network call and
	# without touching the prefix, so it stops before the release is resolved:
	# resolving "latest" is itself a request. It still reports the toolchain
	# step, which is the part an operator sizing an install wants to see.
	if [ "$ctx_dry_run" = yes ]; then
		ctx_log "dry run: would install codectx $ctx_version for $ctx_os/$ctx_arch into $ctx_prefix"
		ctx_install_tools
		return 0
	fi

	ctx_resolve_version

	ctx_tmp="$(mktemp -d)" || ctx_die "could not create a temporary directory"
	trap ctx_cleanup EXIT INT TERM

	ctx_log "resolving release $ctx_tag for $ctx_os/$ctx_arch"
	curl -fsSL -o "$ctx_tmp/checksums.txt" "$ctx_releases_url/download/$ctx_tag/checksums.txt" ||
		ctx_die "could not download checksums.txt for release $ctx_tag"

	if [ "$ctx_bundle" = yes ]; then
		ctx_archive="codectx-bundle_${ctx_version}_${ctx_os}_${ctx_arch}.tar.gz"
	else
		ctx_archive="codectx_${ctx_version}_${ctx_os}_${ctx_arch}.tar.gz"
	fi
	ctx_download_verified "$ctx_archive"

	# Only now, with the digest matched, is anything unpacked.
	mkdir -p "$ctx_tmp/unpacked"
	tar -xzf "$ctx_tmp/$ctx_archive" -C "$ctx_tmp/unpacked" ||
		ctx_die "could not unpack $ctx_archive"

	ctx_binary="$(find "$ctx_tmp/unpacked" -type f -name codectx 2>/dev/null | head -n 1)"
	[ -n "$ctx_binary" ] || ctx_die "$ctx_archive does not contain a codectx binary"

	if [ "$ctx_bundle" = yes ]; then
		ctx_store="$(find "$ctx_tmp/unpacked" -type d -name tools 2>/dev/null | head -n 1)"
		[ -n "$ctx_store" ] ||
			ctx_die "$ctx_archive does not contain a tool store; it is not an offline bundle"
	fi

	mkdir -p "$ctx_prefix" || ctx_die "could not create $ctx_prefix"
	cp "$ctx_binary" "$ctx_prefix/codectx.tmp" ||
		ctx_die "could not write to $ctx_prefix"
	chmod 0755 "$ctx_prefix/codectx.tmp"
	mv "$ctx_prefix/codectx.tmp" "$ctx_prefix/codectx"
	ctx_log "installed codectx $ctx_version to $ctx_prefix/codectx"

	if [ "$ctx_bundle" = yes ]; then
		ctx_store_target="$(ctx_store_dir)"
		mkdir -p "$ctx_store_target" || ctx_die "could not create $ctx_store_target"
		cp -R "$ctx_store/." "$ctx_store_target/" ||
			ctx_die "could not copy the bundled tool store into $ctx_store_target"
		chmod 0700 "$ctx_store_target"
		ctx_log "installed the bundled tool store to $ctx_store_target"
		ctx_log "that is the store codectx reads by default, so nothing needs configuring.
To refuse every network fetch as well, add to ${XDG_CONFIG_HOME:-${HOME:-}/.config}/codectx/config.toml:

  [tools]
  offline = true
"
	fi

	ctx_install_tools

	case ":${PATH:-}:" in
	*":$ctx_prefix:"*) ;;
	*) ctx_log "$ctx_prefix is not on PATH; add it, or run $ctx_prefix/codectx directly" ;;
	esac

	ctx_log "run: codectx version --json"
}

ctx_main "$@"
