#!/usr/bin/env bash
# e2e runner: spins up a real doco-cd (built from the working tree) plus a
# local git server, then runs scenario scripts against them.
#
#   ./test/e2e/run.sh                       # all scenarios
#   ./test/e2e/run.sh failed-deploy-retry   # one scenario
#   KEEP=1 ./test/e2e/run.sh <scenario>     # keep the harness up for debugging
#
# Each scenario is a directory under scenarios/ with:
#   fixture/  - initial content of the polled repo (committed and pushed as v1
#               before the daemon starts)
#   run.sh    - assertions and further repo mutations, sources $E2E_LIB
#   stacks    - names of compose projects the scenario deploys, one per line
#               (used for cleanup)
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export E2E_DIR
export E2E_LIB="$E2E_DIR/lib.sh"

# shellcheck source=lib.sh
source "$E2E_LIB"

cd "$E2E_DIR"

scenarios=("$@")
if [ ${#scenarios[@]} -eq 0 ]; then
	for d in scenarios/*/; do
		scenarios+=("$(basename "$d")")
	done
fi

cleanup_scenario() {
	local scenario=$1

	if [ -f "scenarios/$scenario/stacks" ]; then
		while IFS= read -r stack; do
			[ -n "$stack" ] || continue
			docker compose -p "$stack" down -v --remove-orphans >/dev/null 2>&1 || true
		done <"scenarios/$scenario/stacks"
	fi

	e2e::compose down -v --remove-orphans >/dev/null 2>&1 || true
}

run_scenario() {
	local scenario=$1
	local work="$E2E_DIR/.work"

	[ -d "scenarios/$scenario" ] || e2e::fail "unknown scenario: $scenario"

	e2e::log "=== scenario: $scenario ==="

	rm -rf "$work"
	mkdir -p "$work/repos" "$work/src"

	# the polled repo: bare on the git server side, working clone for pushes
	git init -q --bare -b main "$work/repos/$scenario.git"
	git init -q -b main "$work/src/$scenario"

	export E2E_WORKDIR="$work/src/$scenario"
	git -C "$E2E_WORKDIR" config user.name "e2e"
	git -C "$E2E_WORKDIR" config user.email "e2e@localhost"
	git -C "$E2E_WORKDIR" remote add origin "$work/repos/$scenario.git"

	cp -R "scenarios/$scenario/fixture/." "$E2E_WORKDIR/"
	e2e::repo_push "e2e: initial fixture"

	cat >"$work/poll.yaml" <<-EOF
		- url: http://gitserver/$scenario.git
		  reference: refs/heads/main
		  interval: 10
	EOF

	e2e::compose up -d --wait

	local result=0
	bash "scenarios/$scenario/run.sh" || result=1

	if [ "$result" -ne 0 ]; then
		e2e::log "--- daemon logs (last 100 lines) ---"
		e2e::daemon_logs | tail -100
	fi

	if [ "${KEEP:-0}" = "1" ]; then
		e2e::log "KEEP=1, harness left running (project $E2E_PROJECT)"
	else
		cleanup_scenario "$scenario"
	fi

	return "$result"
}

e2e::log "building harness images"
e2e::compose build

failed=0
for scenario in "${scenarios[@]}"; do
	run_scenario "$scenario" || failed=1
done

if [ "$failed" -ne 0 ]; then
	e2e::fail "some scenarios failed"
fi

e2e::log "all scenarios passed"
