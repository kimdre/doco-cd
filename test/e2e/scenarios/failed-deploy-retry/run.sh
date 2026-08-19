#!/usr/bin/env bash
# Scenario for discussion #1702: a deployment that fails AFTER containers were
# created (post_start hook exits 1) must be recorded, retried on every poll
# with a full recreate, and the marker must clear on the first success.
# Before the fix the daemon logged "no changes detected, skipping deployment"
# forever and the failure was reported exactly once.
set -euo pipefail

# shellcheck source=../../lib.sh
source "$E2E_LIB"

e2e::wait_for 120 "first deployment attempt failed" \
	e2e::daemon_has_log "deployment failed"

e2e::wait_for 30 "failure marker recorded in data volume" \
	e2e::marker_present

cid_after_fail="$(e2e::container_id e2e-retry app)"
[ -n "$cid_after_fail" ] ||
	e2e::fail "app container must exist after the failed deploy (hook fails after start)"

# the regression: same commit must NOT be skipped as already deployed
e2e::wait_for 60 "retry detected on unchanged commit" \
	e2e::daemon_has_log "last deployment attempt failed, retrying"

recreated() {
	local cid
	cid="$(e2e::container_id e2e-retry app)"
	[ -n "$cid" ] && [ "$cid" != "$cid_after_fail" ]
}

e2e::wait_for 90 "retry force-recreated the stack" recreated

# fix the hook, expect success to clear the marker and stop the retries
sed -i.bak 's/HOOK_EXIT: "1"/HOOK_EXIT: "0"/' "$E2E_WORKDIR/.doco-cd.yml"
rm -f "$E2E_WORKDIR/.doco-cd.yml.bak"
e2e::repo_push "fix the hook"

e2e::wait_for 120 "marker cleared after successful deploy" \
	e2e::marker_absent

cid_ok="$(e2e::container_id e2e-retry app)"
[ -n "$cid_ok" ] || e2e::fail "app container must run after the successful deploy"

sleep 25 # two poll cycles

[ "$(e2e::container_id e2e-retry app)" = "$cid_ok" ] ||
	e2e::fail "stack must stay untouched once the deploy succeeded"

e2e::log "scenario passed"
