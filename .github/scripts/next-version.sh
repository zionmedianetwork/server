#!/usr/bin/env bash
#
# next-version.sh — compute the next release tag for this Go module.
#
# This is the only part of the release pipeline that can silently produce a
# WRONG permanent artefact. A broken build step turns a run red and nobody is
# harmed; a tag is immutable the moment it is fetched by a proxy, and the Go
# module proxy fetches aggressively. So the logic lives in a file that can be
# executed and tested outside GitHub Actions rather than inline in the YAML,
# and .github/scripts/next-version-test.sh exercises it over a table of cases.
#
# Usage:
#   next-version.sh <patch|minor|major> [true|false]
#                    ^ release type      ^ cut a release candidate instead
#
# Inputs (all overridable so the script is testable without a repository):
#   TAGS      newline-separated tag list. Default: `git tag --list`.
#   GO_MOD    path to go.mod.            Default: <repo root>/go.mod.
#
# Output:
#   stdout        key=value lines, and the same lines appended to $GITHUB_OUTPUT
#                 when that variable is set.
#   stderr        the human-readable explanation of what it decided and why.
#   exit status   0 on success, 1 on a refusal (with the reason on stderr).
#
set -euo pipefail

die() {
	printf '%s\n' "$*" >&2
	# ::error:: makes the message an annotation in the Actions UI as well as a
	# log line; harmless noise when running locally.
	if [ -n "${GITHUB_ACTIONS:-}" ]; then
		printf '::error::%s\n' "$(printf '%s' "$*" | head -n1)" >&2
	fi
	exit 1
}

note() { printf '%s\n' "$*" >&2; }

emit() {
	printf '%s=%s\n' "$1" "$2"
	if [ -n "${GITHUB_OUTPUT:-}" ]; then
		printf '%s=%s\n' "$1" "$2" >>"$GITHUB_OUTPUT"
	fi
}

# --------------------------------------------------------------------------
# Arguments
# --------------------------------------------------------------------------

release_type="${1:-}"
prerelease="${2:-false}"

case "$release_type" in
patch | minor | major) ;;
*) die "release_type must be one of patch, minor, major (got: '${release_type}')" ;;
esac

case "$prerelease" in
true | false) ;;
*) die "prerelease must be 'true' or 'false' (got: '${prerelease}')" ;;
esac

# --------------------------------------------------------------------------
# The module path, and the major version it declares
# --------------------------------------------------------------------------

go_mod="${GO_MOD:-}"
if [ -z "$go_mod" ]; then
	go_mod="$(git rev-parse --show-toplevel)/go.mod"
fi
[ -f "$go_mod" ] || die "no go.mod at ${go_mod}"

module_path="$(awk '$1 == "module" { print $2; exit }' "$go_mod")"
[ -n "$module_path" ] || die "no 'module' directive in ${go_mod}"

# Go's rule: v0 and v1 share the unsuffixed module path; every major from 2
# onwards must carry a /vN suffix that matches. `/v0` and `/v1` suffixes are
# not merely unconventional, the go command rejects them outright.
if [[ "$module_path" =~ /v([0-9]+)$ ]]; then
	module_major="${BASH_REMATCH[1]}"
	if [ "$module_major" -lt 2 ]; then
		die "go.mod declares module path '${module_path}'. A /v0 or /v1 suffix is invalid: v0 and v1 use the unsuffixed path."
	fi
else
	module_major=1
fi

# --------------------------------------------------------------------------
# The current version
# --------------------------------------------------------------------------

if [ -n "${TAGS+x}" ]; then
	all_tags="$TAGS"
else
	all_tags="$(git tag --list)"
fi

# Strict semver, and strict on purpose:
#
#   - the leading `v` is required, because that is what the Go tooling reads;
#   - all three components are required, so `v1.2` is not a release here;
#   - leading zeros are rejected, because semver forbids them AND because
#     `$(( 08 + 1 ))` is a bash error, not 9. A tag like `v1.08.0` would
#     otherwise crash the arithmetic below or, worse, be read as octal.
#
# Anything that does not match is not a release of this module and is ignored:
# `latest`, `1.2.3`, `release-2`, `v1.2`, `v1.02.3`.
semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

stable_tags="$(printf '%s\n' "$all_tags" | grep -E "$semver_re" || true)"

ignored="$(printf '%s\n' "$all_tags" | grep -v -E "$semver_re" | grep -v '^$' || true)"
if [ -n "$ignored" ]; then
	note "Ignoring $(printf '%s\n' "$ignored" | wc -l | tr -d ' ') non-semver tag(s): $(printf '%s' "$ignored" | tr '\n' ' ')"
fi

if [ -z "$stable_tags" ]; then
	current=""
	cur_major=0
	cur_minor=0
	cur_patch=0
	note "No existing semver tag. Treating the current version as v0.0.0 (unreleased)."
else
	# sort -V mis-orders prereleases across implementations, and git's own
	# --sort=v:refname depends on versionsort.suffix being configured. Sorting
	# the three numeric fields explicitly is portable and has no such corners.
	latest="$(printf '%s\n' "$stable_tags" | sed 's/^v//' | sort -t. -k1,1n -k2,2n -k3,3n | tail -n1)"
	current="v${latest}"
	IFS=. read -r cur_major cur_minor cur_patch <<<"$latest"
	note "Latest release tag: ${current}"
fi

# Prerelease tags are deliberately NOT candidates for "current". Semver gives a
# prerelease lower precedence than the release it precedes, and `go get @latest`
# skips them, so v1.3.0-rc.2 does not mean v1.3.0 has shipped.

# --------------------------------------------------------------------------
# The next version
# --------------------------------------------------------------------------

# 10# forces base 10 even if a value ever arrives with a leading zero.
case "$release_type" in
patch)
	next_major=$((10#$cur_major))
	next_minor=$((10#$cur_minor))
	next_patch=$((10#$cur_patch + 1))
	;;
minor)
	next_major=$((10#$cur_major))
	next_minor=$((10#$cur_minor + 1))
	next_patch=0
	;;
major)
	next_major=$((10#$cur_major + 1))
	next_minor=0
	next_patch=0
	;;
esac

target="v${next_major}.${next_minor}.${next_patch}"

# --------------------------------------------------------------------------
# The v0 / v1 / vN commitments
# --------------------------------------------------------------------------

if [ "$cur_major" -eq 0 ] && [ "$next_major" -eq 1 ]; then
	note ""
	note "NOTE: this is the v0 -> v1.0.0 step, which is a compatibility commitment."
	note "      From v1.0.0 on, a breaking change requires a new MAJOR version, and"
	note "      a new major requires a new module path (${module_path}/v2)."
	note "      Under semver, v0.x explicitly permits breaking changes in a MINOR"
	note "      bump: if this release is 'more breaking changes, still settling',"
	note "      the correct choice is 'minor' (-> v0.$((10#$cur_minor + 1)).0), not 'major'."
	note ""
fi

# The mistake this whole script exists to prevent. Tagging v2.0.0 while go.mod
# still says the unsuffixed path produces a module that `go get module@v2.0.0`
# refuses ("module declares its path as X, but was required as X/v2") and that
# the proxy will happily cache forever.
if [ "$next_major" -ge 2 ] && [ "$module_major" -ne "$next_major" ]; then
	die "$(
		cat <<EOF
Refusing to tag ${target}: go.mod declares 'module ${module_path}'.

Go requires major version 2 and above to be part of the module path. A
${target} tag on this module path produces something no consumer can depend
on correctly:

    go: github.com/.../server@${target}: module declares its path as
        ${module_path}, but was required as ${module_path}/v${next_major}

To release ${target}, in a normal pull request, before running this workflow:

  1. go.mod:   module ${module_path}/v${next_major}
  2. update every internal import of ${module_path}
     (examples/ imports the root package) to ${module_path}/v${next_major}
  3. update the 'go get' line and import paths in README.md
  4. go mod tidy && go build ./... && go test ./...

Then re-run this workflow with release_type=major. This step is a deliberate
source change with its own review; it is not something a release button should
do on your behalf.
EOF
	)"
fi

if [ "$next_major" -le 1 ] && [ "$module_major" -ne 1 ]; then
	die "Refusing to tag ${target}: go.mod declares 'module ${module_path}', which is the v${module_major} module. Tag v${module_major}.x.y from it, not ${target}."
fi

# --------------------------------------------------------------------------
# Release candidates
# --------------------------------------------------------------------------

is_prerelease=false
next_tag="$target"

if [ "$prerelease" = "true" ]; then
	is_prerelease=true
	# Escape the dots so the target is matched literally.
	esc="${target//./\\.}"
	rc_tags="$(printf '%s\n' "$all_tags" | grep -E "^${esc}-rc\.(0|[1-9][0-9]*)$" || true)"
	if [ -z "$rc_tags" ]; then
		rc_num=1
	else
		highest="$(printf '%s\n' "$rc_tags" | sed "s/^${esc}-rc\.//" | sort -n | tail -n1)"
		rc_num=$((10#$highest + 1))
	fi
	next_tag="${target}-rc.${rc_num}"
	note "Release candidate: ${next_tag} (precedes ${target}; 'go get @latest' will not select it)."
fi

# --------------------------------------------------------------------------
# Refusals
# --------------------------------------------------------------------------

if printf '%s\n' "$all_tags" | grep -Fxq "$next_tag"; then
	die "Refusing to tag ${next_tag}: that tag already exists. Tags are immutable once the module proxy has seen them; delete-and-retag is not a recovery path."
fi

# --------------------------------------------------------------------------
# Result
# --------------------------------------------------------------------------

kind="$release_type"
if [ "$is_prerelease" = true ]; then
	kind="${kind}, release candidate"
fi
v1_commitment=false
if [ "$cur_major" -eq 0 ] && [ "$next_major" -eq 1 ]; then
	v1_commitment=true
fi

note "${current:-(none)} -> ${next_tag}  (${kind})"

emit "module_path" "$module_path"
emit "current" "${current:-}"
emit "current_display" "${current:-(none)}"
emit "next" "$next_tag"
emit "next_version" "${next_tag#v}"
emit "target_release" "$target"
emit "is_prerelease" "$is_prerelease"
emit "is_v1_commitment" "$v1_commitment"
