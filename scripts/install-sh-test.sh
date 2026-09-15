#!/bin/sh
# install-sh-test.sh -- what install.sh promises about the toolchain step.
#
# The installer's job is no longer "copy a binary": it must leave the machine
# with the pinned analyzers installed, because a product whose first index stops
# to fetch gigabytes does not work off the bat. That is shell with no compiler
# behind it, so this asserts the things about it that a typo would silently
# break:
#
#   1. a plain install runs `tools prefetch --all` with the binary it installed;
#   2. --tools-for-repo installs that repository's subset instead;
#   3. --no-tools skips the step and says each tool installs on demand; and
#   4. --no-tools and --tools-for-repo together are a refusal, not a
#      silently-ignored word; and
#   5. the tool store install.sh resolves is the one the product resolves,
#      including for a relative XDG_DATA_HOME, which both ignore.
#
# Every case runs --dry-run, which by contract fetches nothing and writes
# nothing, so the test needs no network and no release.
#
# usage: sh scripts/install-sh-test.sh [path/to/install.sh]
set -eu

script="${1:-$(dirname "$0")/../install.sh}"
[ -f "$script" ] || { echo "install-sh-test: no install.sh at $script" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT INT TERM
prefix="$work/bin"
failures=0

# run captures stdout+stderr of one dry run; install.sh logs to stderr.
run() { sh "$script" --dry-run --prefix "$prefix" "$@" 2>&1 || true; }

expect() {
	label="$1"
	want="$2"
	got="$3"
	case "$got" in
	*"$want"*) printf 'ok   %s\n' "$label" ;;
	*)
		printf 'FAIL %s\n  want substring: %s\n  got: %s\n' "$label" "$want" "$got" >&2
		failures=$((failures + 1))
		;;
	esac
}

out="$(run)"
expect "a plain install prefetches every pinned tool" \
	"would run $prefix/codectx tools prefetch --all" "$out"

out="$(run --tools-for-repo /some/repo)"
expect "--tools-for-repo prefetches that repository's subset" \
	"would run $prefix/codectx tools prefetch --for-repo /some/repo" "$out"
case "$out" in
*"prefetch --all"*)
	printf 'FAIL --tools-for-repo must not run prefetch --all\n' >&2
	failures=$((failures + 1))
	;;
esac

out="$(run --no-tools)"
expect "--no-tools skips the toolchain" "skipping the toolchain" "$out"
case "$out" in
*"tools prefetch"*)
	printf 'FAIL --no-tools still planned a prefetch\n  got: %s\n' "$out" >&2
	failures=$((failures + 1))
	;;
*) printf 'ok   --no-tools plans no prefetch\n' ;;
esac

out="$(run --no-tools --tools-for-repo /some/repo)"
expect "--no-tools with --tools-for-repo is refused" "cannot be combined" "$out"

# A dry run must not create the prefix or install anything into it.
if [ -e "$prefix" ]; then
	printf 'FAIL --dry-run created %s\n' "$prefix" >&2
	failures=$((failures + 1))
else
	printf 'ok   --dry-run writes nothing\n'
fi

# 5. The store the installer resolves is the one the product reads. install.sh
# and internal/config.osUserDataDir must agree byte for byte: the product
# honours XDG_DATA_HOME only when it is absolute, so an installer that honoured
# a relative one would copy an offline bundle into a directory nothing reads.
# The help text prints the resolved path through the same function the bundle
# copy uses, so asserting on it asserts the rule.
out="$(HOME=/home/tester XDG_DATA_HOME=relshare sh "$script" --help 2>&1 || true)"
expect "a relative XDG_DATA_HOME is ignored, as the product ignores it" \
	"/home/tester/.local/share/codectx/tools" "$out"
case "$out" in
*"relshare"*)
	printf 'FAIL install.sh honoured a relative XDG_DATA_HOME the product ignores\n  got: %s\n' "$out" >&2
	failures=$((failures + 1))
	;;
esac

out="$(HOME=/home/tester XDG_DATA_HOME=/abs/share sh "$script" --help 2>&1 || true)"
expect "an absolute XDG_DATA_HOME names the store" "/abs/share/codectx/tools" "$out"

if [ "$failures" -ne 0 ]; then
	printf '\ninstall-sh-test: %s failure(s)\n' "$failures" >&2
	exit 1
fi
printf '\ninstall-sh-test: all checks passed\n'
