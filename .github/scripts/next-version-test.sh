#!/usr/bin/env bash
#
# next-version-test.sh — a table-driven test for next-version.sh.
#
# Runs offline: every case supplies its own tag list and its own go.mod, so
# nothing here touches the repository's real tags. Run it by hand after any
# change to next-version.sh:
#
#   bash .github/scripts/next-version-test.sh
#
# It is not wired into CI on purpose: CI's job is to gate merges to the
# library, and adding a shell test there widens that gate for a file only the
# release workflow reads. Run it when you change the release logic.
#
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="${here}/next-version.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

mkmod() { printf 'module %s\n\ngo 1.23.3\n' "$1" >"$2"; }

# check <name> <tags> <module path> <release type> <prerelease> <expect> ...
# <expect> is either "ok:<tag>" or "fail:<substring the stderr must contain>"
check() {
	local name="$1" tags="$2" modpath="$3" rtype="$4" pre="$5" expect="$6"
	local gomod="${tmp}/go.mod"
	mkmod "$modpath" "$gomod"

	local out err status
	out="$(TAGS="$tags" GO_MOD="$gomod" bash "$script" "$rtype" "$pre" 2>"${tmp}/err")"
	status=$?
	err="$(cat "${tmp}/err")"

	local got
	if [ "$status" -eq 0 ]; then
		got="ok:$(printf '%s\n' "$out" | sed -n 's/^next=//p')"
	else
		got="fail"
	fi

	local ok=false
	case "$expect" in
	fail:*)
		if [ "$status" -ne 0 ] && printf '%s' "$err" | grep -qF "${expect#fail:}"; then
			ok=true
		fi
		;;
	*)
		if [ "$got" = "$expect" ]; then ok=true; fi
		;;
	esac

	if [ "$ok" = true ]; then
		pass=$((pass + 1))
		printf 'PASS  %-58s %s\n' "$name" "${got}"
	else
		fail=$((fail + 1))
		printf 'FAIL  %-58s want=%s got=%s status=%s\n' "$name" "$expect" "$got" "$status"
		printf '      stderr: %s\n' "$(printf '%s' "$err" | tr '\n' '|')"
	fi
}

MOD='github.com/zionmedianetwork/server'
MOD2="${MOD}/v2"
MOD3="${MOD}/v3"

echo "=== no tags at all ==============================================="
check "no tags, patch"                   ""        "$MOD" patch false "ok:v0.0.1"
check "no tags, minor"                   ""        "$MOD" minor false "ok:v0.1.0"
check "no tags, major"                   ""        "$MOD" major false "ok:v1.0.0"

echo
echo "=== v0.0.1 (this repository today) ==============================="
check "v0.0.1, patch"                    "v0.0.1"  "$MOD" patch false "ok:v0.0.2"
check "v0.0.1, minor"                    "v0.0.1"  "$MOD" minor false "ok:v0.1.0"
check "v0.0.1, major"                    "v0.0.1"  "$MOD" major false "ok:v1.0.0"

echo
echo "=== v0.9.9 (v0 does not roll over into v1 by accident) ==========="
check "v0.9.9, patch"                    "v0.9.9"  "$MOD" patch false "ok:v0.9.10"
check "v0.9.9, minor"                    "v0.9.9"  "$MOD" minor false "ok:v0.10.0"
check "v0.9.9, major"                    "v0.9.9"  "$MOD" major false "ok:v1.0.0"

echo
echo "=== v1.2.3 (major must be refused on an unsuffixed module path) =="
check "v1.2.3, patch"                    "v1.2.3"  "$MOD" patch false "ok:v1.2.4"
check "v1.2.3, minor"                    "v1.2.3"  "$MOD" minor false "ok:v1.3.0"
check "v1.2.3, major -> v2 refusal"      "v1.2.3"  "$MOD" major false "fail:Go requires major version 2 and above to be part of the module path"

echo
echo "=== the same major, with go.mod moved to /v2 ====================="
check "v1.2.3 + /v2 go.mod, major"       "v1.2.3"  "$MOD2" major false "ok:v2.0.0"
check "v2.0.0 + /v2 go.mod, patch"       "v2.0.0"  "$MOD2" patch false "ok:v2.0.1"
check "v2.4.1 + /v2 go.mod, minor"       "v2.4.1"  "$MOD2" minor false "ok:v2.5.0"
check "v2.4.1 + /v2 go.mod, major"       "v2.4.1"  "$MOD2" major false "fail:but was required as"
check "v2.4.1 + /v3 go.mod, major"       "v2.4.1"  "$MOD3" major false "ok:v3.0.0"
check "v1.2.3 + /v3 go.mod, major"       "v1.2.3"  "$MOD3" major false "fail:Refusing to tag v2.0.0"
check "/v2 go.mod but tagging v1"        "v0.5.0"  "$MOD2" minor false "fail:which is the v2 module"
check "/v1 suffix is itself invalid"     "v1.0.0"  "${MOD}/v1" patch false "fail:A /v0 or /v1 suffix is invalid"

echo
echo "=== non-semver tags are ignored =================================="
NOISE=$'latest\nv1.2.3\nrelease-2\n1.2.3\nv1.2\nv1.2.3.4\nv1.02.3\nfoo/bar\nv0.0.1'
check "noise + v1.2.3, patch"            "$NOISE"  "$MOD" patch false "ok:v1.2.4"
check "noise + v1.2.3, minor"            "$NOISE"  "$MOD" minor false "ok:v1.3.0"
check "only non-semver tags, patch"      $'latest\nnightly\nrelease-2' "$MOD" patch false "ok:v0.0.1"
check "leading zero tag is not a release" $'v1.02.3\nv1.1.0' "$MOD" patch false "ok:v1.1.1"

echo
echo "=== ordering is numeric, not lexicographic ======================="
check "v0.9.0 vs v0.10.0"                $'v0.9.0\nv0.10.0' "$MOD" patch false "ok:v0.10.1"
check "v0.2.0 vs v0.10.0 vs v0.1.0"      $'v0.2.0\nv0.10.0\nv0.1.0' "$MOD" minor false "ok:v0.11.0"
check "v9.0.0 vs v10.0.0 (/v10 go.mod)"  $'v9.0.0\nv10.0.0' "${MOD}/v10" patch false "ok:v10.0.1"
check "prerelease is not the baseline"   $'v1.2.3\nv1.3.0-rc.1' "$MOD" patch false "ok:v1.2.4"

echo
echo "=== release candidates ==========================================="
check "rc, no tags, minor"               ""        "$MOD" minor true  "ok:v0.1.0-rc.1"
check "rc, v0.0.1, major"                "v0.0.1"  "$MOD" major true  "ok:v1.0.0-rc.1"
check "rc, second candidate"             $'v0.0.1\nv1.0.0-rc.1' "$MOD" major true "ok:v1.0.0-rc.2"
check "rc, tenth candidate sorts right"  $'v0.0.1\nv1.0.0-rc.9\nv1.0.0-rc.10' "$MOD" major true "ok:v1.0.0-rc.11"
check "rc of a different target ignored" $'v0.0.1\nv0.1.0-rc.4' "$MOD" major true "ok:v1.0.0-rc.1"
check "promoting an rc to the release"   $'v0.0.1\nv1.0.0-rc.2' "$MOD" major false "ok:v1.0.0"
check "rc refused on v2 path mismatch"   "v1.2.3"  "$MOD" major true  "fail:Refusing to tag v2.0.0"

echo
echo "=== the computed tag can never be one that already exists ========"
# The script's duplicate-tag guard is defence in depth and is UNREACHABLE from
# a complete tag list: the baseline is always the highest stable tag, so its
# successor cannot already be present, and an rc number is always one past the
# highest rc for the same target. The property that matters is that repeated
# runs are strictly monotonic -- which is what these cases assert, by feeding
# the previous run's output back in as an existing tag.
#
# The guard still earns its place because the tag list is not always complete:
# a shallow or tag-less fetch, or a tag pushed by someone else between the plan
# and the push, both produce a list that is missing entries. That case is
# caught authoritatively by `git ls-remote` in release.yml immediately before
# the tag is created, not here.
check "run 1 of 3: minor from v0.0.1"    $'v0.0.1' "$MOD" minor false "ok:v0.1.0"
check "run 2 of 3: minor again"          $'v0.0.1\nv0.1.0' "$MOD" minor false "ok:v0.2.0"
check "run 3 of 3: patch after that"     $'v0.0.1\nv0.1.0\nv0.2.0' "$MOD" patch false "ok:v0.2.1"
check "rc runs are monotonic too"        $'v0.0.1\nv0.1.0-rc.1\nv0.1.0-rc.2' "$MOD" minor true "ok:v0.1.0-rc.3"

echo
echo "=== bad arguments ================================================"
check "unknown release type"             "v1.0.0"  "$MOD" release false "fail:release_type must be one of"
check "empty release type"               "v1.0.0"  "$MOD" ""      false "fail:release_type must be one of"
check "non-boolean prerelease"           "v1.0.0"  "$MOD" patch   yes   "fail:prerelease must be"

echo
printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
