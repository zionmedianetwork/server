#!/usr/bin/env bash
#
# warm-proxy-test.sh — a table-driven test for warm-proxy.sh.
#
# Runs offline. Every case supplies its own go.mod, and the proxy is either a
# closed port on localhost (for the URL and give-up cases) or a throwaway
# `python3 -m http.server` serving a directory laid out like the module proxy
# (for the verification cases). Nothing here touches proxy.golang.org, so it is
# fast, deterministic and safe to run in a loop:
#
#   bash .github/scripts/warm-proxy-test.sh
#
# The live counterpart is one command, and it is idempotent — fetching a version
# the proxy already has changes nothing:
#
#   bash .github/scripts/warm-proxy.sh v0.1.0
#
# Like next-version-test.sh, this is deliberately not wired into CI: CI gates
# merges to the library, and this covers a file only the release workflow reads.
# Run it when you change warm-proxy.sh.
#
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="${here}/warm-proxy.sh"
tmp="$(mktemp -d)"

# A port nothing listens on, so "the proxy did not answer" is immediate and
# needs no network. Port 1 is privileged and unused; the connection is refused
# by the kernel in microseconds.
DEAD='http://127.0.0.1:1'

server_pid=""
cleanup() {
	if [ -n "$server_pid" ]; then
		kill "$server_pid" 2>/dev/null
		wait "$server_pid" 2>/dev/null
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT

pass=0
fail=0
last_out=""
last_status=0

# Defaults for a run; each case overrides what it cares about.
gomod="${tmp}/go.mod"
proxy="$DEAD"
attempts=1
delay=0
timeout=5
expect_commit=""

mkmod() { printf 'module %s\n\ngo 1.23.3\n' "$1" >"$gomod"; }

run_warm() {
	last_out="$(GO_MOD="$gomod" PROXY="$proxy" ATTEMPTS="$attempts" DELAY="$delay" \
		TIMEOUT="$timeout" EXPECT_COMMIT="$expect_commit" \
		bash "$script" "$@" 2>&1)"
	last_status=$?
}

report() {
	local name="$1" ok="$2" detail="${3:-}"
	if [ "$ok" = true ]; then
		pass=$((pass + 1))
		printf 'PASS  %-52s %s\n' "$name" "$detail"
	else
		fail=$((fail + 1))
		printf 'FAIL  %-52s %s\n' "$name" "$detail"
		printf '%s\n' "$last_out" | sed 's/^/      | /'
	fi
}

# Every case asserts this, everywhere, because it is the script's entire
# contract: a warm-up cannot fail a release that has already happened.
assert_zero_exit() {
	if [ "$last_status" -ne 0 ]; then
		report "$1 (exit status)" false "want=0 got=${last_status}"
		return 1
	fi
	return 0
}

# check_url <name> <module path in go.mod> <version> <expected escaped path>
#
# Drives the real script with a dead proxy and reads back the URL it was about
# to request, so the case-encoding is tested through the code that ships rather
# than through a copy of it.
check_url() {
	local name="$1" modpath="$2" version="$3" want="$4"
	mkmod "$modpath"
	proxy="$DEAD" attempts=1 delay=0 timeout=5 expect_commit=""
	run_warm "$version"

	local got want_url ok=false
	got="$(printf '%s\n' "$last_out" | sed -n 's/^Warming: //p')"
	want_url="${DEAD}/${want}"
	if [ "$got" = "$want_url" ] && [ "$last_status" -eq 0 ]; then ok=true; fi
	report "$name" "$ok" "${got#"${DEAD}"/}"
}

echo "=== module path case-encoding (!x for each uppercase X) ================"
# github.com/Foo/Bar -> github.com/!foo/!bar. This module's own path has no
# capitals, so none of this bites today; it bites the day the module is renamed
# or gains a suffix, and the symptom would be a 404 reading "invalid escaped
# module path" from a step nobody looks at.
check_url "this module, as it is today" \
	'github.com/zionmedianetwork/server' v0.1.0 \
	'github.com/zionmedianetwork/server/@v/v0.1.0.info'
check_url "the spec's own example" \
	'github.com/Foo/Bar' v1.2.3 \
	'github.com/!foo/!bar/@v/v1.2.3.info'
check_url "a real one: BurntSushi/toml" \
	'github.com/BurntSushi/toml' v1.4.0 \
	'github.com/!burnt!sushi/toml/@v/v1.4.0.info'
check_url "consecutive capitals" \
	'example.com/ABC/def' v1.0.0 \
	'example.com/!a!b!c/def/@v/v1.0.0.info'
check_url "capital at the very start" \
	'Example.com/x' v1.0.0 \
	'!example.com/x/@v/v1.0.0.info'
check_url "capitals around punctuation" \
	'github.com/A/b-C_d.E' v1.0.0 \
	'github.com/!a/b-!c_d.!e/@v/v1.0.0.info'
check_url "a /vN suffix, lowercase v" \
	'github.com/zionmedianetwork/server/v2' v2.0.0 \
	'github.com/zionmedianetwork/server/v2/@v/v2.0.0.info'
check_url "a /vN suffix with capitals in the path" \
	'github.com/Foo/Bar/v2' v2.0.0 \
	'github.com/!foo/!bar/v2/@v/v2.0.0.info'
check_url "a two-digit major" \
	'github.com/Foo/Bar/v10' v10.3.1 \
	'github.com/!foo/!bar/v10/@v/v10.3.1.info'
check_url "a capital V in the suffix (go would reject it; encode it anyway)" \
	'example.com/x/V3' v3.0.0 \
	'example.com/x/!v3/@v/v3.0.0.info'
check_url "gopkg.in style, no capitals to encode" \
	'gopkg.in/yaml.v3' v3.0.1 \
	'gopkg.in/yaml.v3/@v/v3.0.1.info'
check_url "a lone capital, nothing else" \
	'example.com/Z' v1.0.0 \
	'example.com/!z/@v/v1.0.0.info'
# The version is escaped by the same rule and for the same reason. Tags cut by
# next-version.sh are lowercase (-rc.N), so this is belt and braces.
check_url "the version is case-encoded too" \
	'example.com/x' v1.0.0-RC.1 \
	'example.com/x/@v/v1.0.0-!r!c.1.info'
check_url "a prerelease as this repo actually spells it" \
	'github.com/zionmedianetwork/server' v1.0.0-rc.1 \
	'github.com/zionmedianetwork/server/@v/v1.0.0-rc.1.info'

echo
echo "=== the proxy never answers ==========================================="

mkmod 'github.com/zionmedianetwork/server'
proxy="$DEAD" attempts=3 delay=0 timeout=5 expect_commit=""
run_warm v1.2.3
if assert_zero_exit "gives up after ATTEMPTS tries"; then
	tries="$(printf '%s\n' "$last_out" | grep -c '^attempt ')"
	ok=false
	if [ "$tries" -eq 3 ] &&
		printf '%s' "$last_out" | grep -q 'did not serve' &&
		printf '%s' "$last_out" | grep -q 'not a failed release'; then ok=true; fi
	report "gives up after ATTEMPTS tries, exit 0" "$ok" "attempts=${tries}"
fi

GITHUB_ACTIONS=true run_warm v1.2.3
ok=false
if [ "$last_status" -eq 0 ] && printf '%s' "$last_out" | grep -q '^::warning::'; then ok=true; fi
report "raises a ::warning:: annotation, not ::error::" "$ok"

# One attempt is the floor: the loop asks before it counts, so no setting of
# ATTEMPTS can make this step silently do nothing.
attempts=0
run_warm v1.2.3
ok=false
if [ "$last_status" -eq 0 ] && [ "$(printf '%s\n' "$last_out" | grep -c '^attempt ')" -eq 1 ]; then ok=true; fi
report "ATTEMPTS=0 still asks once and terminates" "$ok"

echo
echo "=== bad inputs are warnings, never failures ==========================="

attempts=1
run_warm
ok=false
if [ "$last_status" -eq 0 ] && printf '%s' "$last_out" | grep -q 'no version argument'; then ok=true; fi
report "no version argument" "$ok"

gomod="${tmp}/nonexistent/go.mod"
run_warm v1.2.3
ok=false
if [ "$last_status" -eq 0 ] && printf '%s' "$last_out" | grep -q 'no go.mod at'; then ok=true; fi
report "go.mod missing" "$ok"

gomod="${tmp}/go.mod"
printf 'go 1.23.3\n' >"$gomod"
run_warm v1.2.3
ok=false
if [ "$last_status" -eq 0 ] && printf '%s' "$last_out" | grep -q "no 'module' directive"; then ok=true; fi
report "go.mod with no module directive" "$ok"

echo
echo "=== a proxy that does answer =========================================="

if ! command -v python3 >/dev/null 2>&1; then
	echo "SKIP  python3 not installed; the verification cases need a local HTTP server"
else
	root="${tmp}/proxy"
	infodir="${root}/github.com/zionmedianetwork/server/@v"
	mkdir -p "$infodir"

	# Started on port 0 and the real port read back, so two copies of this test
	# cannot collide and no port is assumed free. -u because the "Serving HTTP
	# on ... port N" line is block-buffered into oblivion when stdout is a file.
	(cd "$root" && exec python3 -u -m http.server 0 --bind 127.0.0.1) >"${tmp}/server.log" 2>&1 &
	server_pid=$!

	port=""
	for _ in $(seq 1 50); do
		port="$(sed -n 's/.*port \([0-9][0-9]*\).*/\1/p' "${tmp}/server.log" | head -n1)"
		[ -n "$port" ] && break
		sleep 0.1
	done

	if [ -z "$port" ]; then
		report "local proxy starts" false "no port in ${tmp}/server.log"
	else
		report "local proxy starts" true "127.0.0.1:${port}"
		mkmod 'github.com/zionmedianetwork/server'
		proxy="http://127.0.0.1:${port}"
		attempts=3
		delay=0
		timeout=5

		good='{"Version":"v1.2.3","Time":"2026-08-10T02:11:54Z","Origin":{"VCS":"git","URL":"https://github.com/zionmedianetwork/server","Hash":"825ef56b4377d914d6a785d40fbff02badd36833","Ref":"refs/tags/v1.2.3"}}'

		# 1. Everything agrees.
		printf '%s' "$good" >"${infodir}/v1.2.3.info"
		expect_commit=825ef56b4377d914d6a785d40fbff02badd36833
		run_warm v1.2.3
		ok=false
		if [ "$last_status" -eq 0 ] &&
			printf '%s' "$last_out" | grep -q 'attempt 1/3: HTTP 200' &&
			printf '%s' "$last_out" | grep -q 'ref=refs/tags/v1.2.3' &&
			printf '%s' "$last_out" | grep -q '### Module proxy$' &&
			! printf '%s' "$last_out" | grep -q 'serving something else'; then ok=true; fi
		report "200 with matching version, ref and commit" "$ok"

		# 2. The proxy is serving this version from a different commit, which is
		#    what a delete-and-retag looks like from the outside. Loud, but the
		#    release has already happened, so still exit 0.
		expect_commit=1111111111111111111111111111111111111111
		GITHUB_ACTIONS=true run_warm v1.2.3
		ok=false
		if [ "$last_status" -eq 0 ] &&
			printf '%s' "$last_out" | grep -q 'serving something else' &&
			printf '%s' "$last_out" | grep -q '^::error::' &&
			printf '%s' "$last_out" | grep -q 'resolved the tag to commit'; then ok=true; fi
		report "commit mismatch: ::error::, still exit 0" "$ok"

		# 3. Same for a body whose Version is not the one asked for.
		printf '%s' '{"Version":"v9.9.9","Origin":{"Ref":"refs/tags/v9.9.9","Hash":"825ef56b4377d914d6a785d40fbff02badd36833"}}' >"${infodir}/v1.2.4.info"
		expect_commit=825ef56b4377d914d6a785d40fbff02badd36833
		run_warm v1.2.4
		ok=false
		if [ "$last_status" -eq 0 ] &&
			printf '%s' "$last_out" | grep -q 'proxy reports version' &&
			printf '%s' "$last_out" | grep -q 'proxy fetched' &&
			printf '%s' "$last_out" | grep -q 'refs/tags/v9.9.9'; then ok=true; fi
		report "version and ref mismatch are both reported" "$ok"

		# 4. No Origin at all: the proxy has entries predating provenance, and
		#    "not stated" must not be read as "wrong".
		printf '%s' '{"Version":"v1.2.5","Time":"2019-01-01T00:00:00Z"}' >"${infodir}/v1.2.5.info"
		run_warm v1.2.5
		ok=false
		if [ "$last_status" -eq 0 ] &&
			printf '%s' "$last_out" | grep -q 'ref=(none) commit=(none)' &&
			! printf '%s' "$last_out" | grep -q 'serving something else'; then ok=true; fi
		report "a body with no Origin is not a mismatch" "$ok"

		# 5. The case this step exists for: not there yet, then there. The file
		#    appears while the script is between attempts.
		expect_commit=825ef56b4377d914d6a785d40fbff02badd36833
		attempts=5
		delay=2
		(
			sleep 3
			printf '%s' "${good//v1.2.3/v1.3.0}" >"${infodir}/v1.3.0.info"
		) &
		waiter=$!
		run_warm v1.3.0
		wait "$waiter" 2>/dev/null
		ok=false
		if [ "$last_status" -eq 0 ] &&
			printf '%s' "$last_out" | grep -q 'HTTP 404' &&
			printf '%s' "$last_out" | grep -q 'HTTP 200' &&
			printf '%s' "$last_out" | grep -q '### Module proxy$'; then ok=true; fi
		report "404 first, then 200 on a retry" "$ok" \
			"$(printf '%s\n' "$last_out" | grep -c '^attempt ') attempt(s)"
	fi
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
