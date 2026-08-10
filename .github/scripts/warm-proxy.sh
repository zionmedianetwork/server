#!/usr/bin/env bash
#
# warm-proxy.sh — ask proxy.golang.org for a version that was just tagged, so
# the proxy fetches it now instead of on the first consumer's `go get`.
#
# Pushing the tag IS the publication for a Go module; there is no upload step
# and nothing to hand to a registry. But the proxy is demand-driven: it only
# learns a version exists when something asks for it, and by default that
# something is whoever runs `go get <module>@<version>` first. They pay for the
# origin fetch, and they are the first to find out if the publication is broken.
# This script is that first asker, on purpose, inside the release run that made
# the tag.
#
# IT NEVER EXITS NON-ZERO. Not on a network error, not on a 404, not on an
# unexpected failure inside itself — see the EXIT trap below. By the time it
# runs, the tag is pushed and the GitHub Release exists, and neither can be
# taken back. A warm-up that fails has changed nothing: the proxy fetches on
# demand, exactly as it did for every release before this script existed. Making
# a finished release go red would report a problem that does not exist, and
# "the release failed" is what a red release job means to everybody who sees it.
#
# Silence is not the alternative. Every failure is printed, raised as a workflow
# annotation, and written to the job summary. The job stays green and the
# operator still finds out.
#
# Usage:
#   warm-proxy.sh <version>
#                  ^ the tag that was just pushed, e.g. v1.2.3 or v1.0.0-rc.1
#
# Inputs (all overridable, so this is testable without cutting a release):
#   GO_MOD         path to go.mod.                  Default: <repo root>/go.mod.
#   PROXY          proxy base URL.                  Default: https://proxy.golang.org.
#   ATTEMPTS       number of requests.              Default: 5.
#   DELAY          seconds between attempts.        Default: 10.
#   TIMEOUT        seconds allowed per attempt.     Default: 30.
#   EXPECT_COMMIT  commit the tag must resolve to.  Default: $GITHUB_SHA.
#
# Output:
#   stdout        one line per attempt, then the verdict.
#   $GITHUB_STEP_SUMMARY, when set, gains a short section saying which.
#   exit status   0. Always. See above.
#
# .github/scripts/warm-proxy-test.sh drives it over the escaping table and both
# failure paths, offline.
#
set -uo pipefail

# --------------------------------------------------------------------------
# Reporting
# --------------------------------------------------------------------------

say() { printf '%s\n' "$*"; }

# An Actions annotation as well as a log line, so a failure is visible on the
# run page without opening the log. Neither level affects the outcome of
# anything: ::warning:: and ::error:: label output, they do not set an exit
# status. warning = "the proxy did not answer, nothing is wrong"; error = "the
# proxy answered with something other than what we just published", which is
# the one finding here worth waking up for.
annotate() {
	local level="$1"
	shift
	printf '%s\n' "$*"
	if [ -n "${GITHUB_ACTIONS:-}" ]; then
		printf '::%s::%s\n' "$level" "$(printf '%s' "$*" | head -n1)"
	fi
}

# A markdown block for the job summary, read from stdin, and echoed to the log
# either way so a local run shows exactly what a real run would publish.
summary() {
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		tee -a "$GITHUB_STEP_SUMMARY"
	else
		cat
	fi
}

workdir="$(mktemp -d)"

# The contract at the top of this file, enforced rather than promised. `set -u`
# meeting an unset variable, a missing curl, an editing mistake in a branch only
# reached on a bad day: all of them land here, get reported, and still leave the
# job green. Step-level `continue-on-error:` would have covered the same cases,
# but it hides the difference between "the proxy did not answer" and "this
# script is broken", and it cannot print an explanation of either.
finish() {
	local rc=$?
	trap - EXIT
	rm -rf "$workdir"
	if [ "$rc" -ne 0 ]; then
		annotate warning "Module proxy warm-up ended unexpectedly (status ${rc}). This is a defect in warm-proxy.sh, not a failed release: the tag and the release are already published and the proxy will fetch the version on first use."
	fi
	exit 0
}
trap finish EXIT

# --------------------------------------------------------------------------
# The module path, case-encoded
# --------------------------------------------------------------------------

# The proxy protocol is case-encoded: every uppercase letter in a module path or
# a version is written as '!' followed by its lowercase form, so that paths
# differing only in case cannot collide in the proxy's storage. Getting it wrong
# is not subtle when it happens —
#
#   $ curl https://proxy.golang.org/github.com/BurntSushi/toml/@v/v1.4.0.info
#   bad request: invalid escaped module path "github.com/BurntSushi/toml"   (404)
#   $ curl 'https://proxy.golang.org/github.com/!burnt!sushi/toml/@v/v1.4.0.info'
#   {"Version":"v1.4.0",...}                                               (200)
#
# — but this module's path is all lowercase, so a hardcoded path would work
# today and break silently on the day someone renames the module or adds a /vN
# suffix with a capital in it. The path is read from go.mod and encoded, so that
# day never comes.
#
# The first expression inserts the '!' before each uppercase letter and the
# second lowercases them, which is safe precisely because those were the only
# uppercase letters in the string. (bash's ${x,,} does it in one expansion, but
# that is bash 4 and macOS ships 3.2; this script runs on a laptop too.)
#
# The alphabet is spelled out rather than written [A-Z] or [:upper:] because the
# rule is defined over ASCII A-Z exactly — golang.org/x/mod/module.EscapePath
# escapes those and rejects anything else — whereas a range or a character class
# can fold accented letters in under another locale.
case_encode() {
	printf '%s' "$1" |
		sed -e 's/[ABCDEFGHIJKLMNOPQRSTUVWXYZ]/!&/g' \
			-e 'y/ABCDEFGHIJKLMNOPQRSTUVWXYZ/abcdefghijklmnopqrstuvwxyz/'
}

# The .info body is a small, flat JSON object:
#
#   {"Version":"v0.1.0","Time":"2026-08-10T02:11:54Z","Origin":{"VCS":"git",
#    "URL":"https://github.com/zionmedianetwork/server",
#    "Hash":"825ef56b4377d914d6a785d40fbff02badd36833","Ref":"refs/tags/v0.1.0"}}
#
# Three string values are wanted out of it, each under a key that occurs at most
# once, so matching the key-and-value pair directly reads them without
# depending on jq being installed and without any parsing worth the name. A
# field this does not find comes back empty, which every caller below treats as
# "not stated" rather than "wrong".
# The trailing tr drops backticks for the same reason as the response detail
# below: these values are quoted into a markdown code span in the job summary.
json_string() {
	grep -o "\"$1\":\"[^\"]*\"" "${workdir}/body" | head -n1 |
		sed -e 's/^[^:]*:"//' -e 's/"$//' | tr -d '\140'
}

version="${1:-}"
if [ -z "$version" ]; then
	annotate warning "warm-proxy.sh: no version argument, nothing to warm."
	exit 0
fi

go_mod="${GO_MOD:-}"
if [ -z "$go_mod" ]; then
	go_mod="$(git rev-parse --show-toplevel 2>/dev/null || true)/go.mod"
fi
if [ ! -f "$go_mod" ]; then
	annotate warning "warm-proxy.sh: no go.mod at '${go_mod}', so the module path is unknown and nothing can be warmed."
	exit 0
fi

module_path="$(awk '$1 == "module" { print $2; exit }' "$go_mod")"
if [ -z "$module_path" ]; then
	annotate warning "warm-proxy.sh: no 'module' directive in ${go_mod}."
	exit 0
fi

proxy="${PROXY:-https://proxy.golang.org}"
url="${proxy%/}/$(case_encode "$module_path")/@v/$(case_encode "$version").info"

# --------------------------------------------------------------------------
# The request
# --------------------------------------------------------------------------
#
# The .info endpoint, and only that one:
#
#   - It is the request that resolves an explicit `go get <module>@<version>`,
#     which is exactly what the release summary tells consumers to run, and it
#     is what forces the proxy's fetch-from-origin on a cache miss. Once it
#     answers, the version is in the proxy and the .mod and .zip come from the
#     same fetch.
#   - Its body carries Version and Origin, so the same request that warms the
#     cache also proves *what* was warmed. A HEAD or a @v/list could not.
#   - @v/list and @latest are computed from a separately cached view of the
#     origin's tags with a TTL of its own. Observed on the v0.1.0 release:
#     @v/list still omitted v0.1.0 well after this exact .info request had
#     returned 200 for it, and caught up later without help. Requesting them
#     would add calls that change nothing, and at worst re-warm a listing that
#     does not have the new tag in it yet.
#   - `GOPROXY=... go list -m <module>@<version>` would issue the same request,
#     but it needs a module context, a toolchain on the runner and a writable
#     module cache, and querying the main module's own path at a version from
#     inside that module is a corner with its own behaviour. curl asks the proxy
#     the one question, with none of that attached.
#
# Retries: the tag was pushed seconds ago, and the proxy has to reach GitHub to
# see it. Five attempts with a flat 10s gap spends at most ~40s waiting (plus up
# to 30s per request), which covers a transient origin fetch without holding the
# `release` concurrency group for minutes. Anything still failing after that is
# an outage at the proxy, which no amount of looping here fixes, and which
# resolves itself the moment a consumer asks. Flat rather than exponential on
# purpose: five requests is not load, so backoff would only make the bound
# harder to state. The loop always makes at least one attempt and always
# terminates.
attempts="${ATTEMPTS:-5}"
delay="${DELAY:-10}"
timeout="${TIMEOUT:-30}"

say "Module:  ${module_path}@${version}"
say "Warming: ${url}"

warmed=false
attempt=1
http=""
detail=""

while :; do
	http="$(curl -sS -o "${workdir}/body" -w '%{http_code}' --max-time "$timeout" "$url" 2>"${workdir}/err")"
	curl_rc=$?

	if [ "$curl_rc" -eq 0 ] && [ "$http" = "200" ]; then
		say "attempt ${attempt}/${attempts}: HTTP 200"
		warmed=true
		break
	fi

	# curl's own message when it never got an answer, the proxy's message when
	# it did — its 404 bodies say which of "unknown revision" and "invalid
	# escaped module path" happened, and that distinction is the whole
	# difference between waiting and having a bug.
	#
	# Truncated, and stripped of backticks and newlines: this is a response body
	# from a third party that ends up inside a code span in the job summary, and
	# markdown it can close is markdown it can rewrite.
	detail="$(head -n1 "${workdir}/err" | tr -d '\140')"
	if [ -z "$detail" ]; then
		detail="$(head -c 200 "${workdir}/body" | tr -d '\n\140')"
	fi
	say "attempt ${attempt}/${attempts}: HTTP ${http:-000}${detail:+ - ${detail}}"

	attempt=$((attempt + 1))
	if [ "$attempt" -gt "$attempts" ]; then
		break
	fi
	sleep "$delay"
done

# --------------------------------------------------------------------------
# Verdict
# --------------------------------------------------------------------------

if [ "$warmed" != true ]; then
	annotate warning "The module proxy did not serve ${module_path}@${version} after ${attempts} attempt(s) (last: HTTP ${http:-000}). The release is complete; the proxy will fetch on first use."
	summary <<-EOF

		### Module proxy — not warmed

		\`${module_path}@${version}\` is tagged and released. \`${proxy}\` did not
		answer for it in ${attempts} attempt(s) — last response: \`HTTP ${http:-000}${detail:+ ${detail}}\`.

		**This is not a failed release, and there is nothing to re-run.** The proxy
		fetches a version the first time anybody asks for it; that is what happened
		for every release before this step existed. The only thing lost is that the
		first \`go get\` pays for the fetch instead of this job. To do it by hand:

		\`\`\`
		curl -fsS ${url}
		\`\`\`
	EOF
	exit 0
fi

# A 200 says the proxy served *something* for this path. These three fields say
# it served what was just published. They cannot fail the job either — by this
# point the tag and the release are immutable, and the useful action is a human
# looking, not a red run — but a mismatch is annotated at error level because it
# is the one outcome here that means something is actually wrong.
got_version="$(json_string Version)"
got_ref="$(json_string Ref)"
got_hash="$(json_string Hash)"
expect_commit="${EXPECT_COMMIT:-${GITHUB_SHA:-}}"

mismatch=""

if [ -n "$got_version" ] && [ "$got_version" != "$version" ]; then
	mismatch="${mismatch}- proxy reports version \`${got_version}\`, expected \`${version}\`"$'\n'
fi

# Origin is absent from entries the proxy cached before it recorded provenance,
# so an empty Ref or Hash is "not stated", not "wrong". Only a stated value that
# disagrees is a finding.
if [ -n "$got_ref" ] && [ "$got_ref" != "refs/tags/${version}" ]; then
	mismatch="${mismatch}- proxy fetched \`${got_ref}\`, expected \`refs/tags/${version}\`"$'\n'
fi

if [ -n "$got_hash" ] && [ -n "$expect_commit" ] && [ "$got_hash" != "$expect_commit" ]; then
	mismatch="${mismatch}- proxy resolved the tag to commit \`${got_hash}\`, this run tagged \`${expect_commit}\`"$'\n'
fi

say "proxy says: version=${got_version:-(none)} ref=${got_ref:-(none)} commit=${got_hash:-(none)}"

if [ -n "$mismatch" ]; then
	annotate error "The module proxy served ${module_path}@${version}, but not what this run published. See the job summary."
	summary <<-EOF

		### Module proxy — warmed, but serving something else

		\`${proxy}\` answered for \`${module_path}@${version}\`, and what it is
		serving does not match this release:

		${mismatch}
		A version the proxy has already cached is fixed forever, so this is not
		fixable by retagging — deleting the tag upstream does not un-publish it.
		The likely causes are a tag of this name that existed and was deleted
		before, or another push winning a race with this one. Check what
		\`${url}\` returns and, if it is wrong, release the next version rather
		than trying to correct this one.
	EOF
	exit 0
fi

summary <<-EOF

	### Module proxy

	\`${proxy}\` is serving \`${module_path}@${version}\`${got_ref:+ from \`${got_ref}\`}${got_hash:+ at \`${got_hash:0:12}\`}.
	The first \`go get\` of it is a cache hit.

	The proxy's \`@v/list\` and the pkg.go.dev index refresh on their own schedule
	and can still lag by minutes — an explicit \`go get ${module_path}@${version}\`
	does not wait for either.
EOF
