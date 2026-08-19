# Helpers for e2e scenarios. Sourced by run.sh and by every scenario script
# (via $E2E_LIB). Everything a scenario needs is here: waiting, log grepping,
# repo pushes, container lookups, daemon state access.

E2E_PROJECT="doco-e2e"
E2E_DAEMON="doco-e2e-daemon"

e2e::compose() {
	docker compose -f "$E2E_DIR/harness/compose.yaml" -p "$E2E_PROJECT" "$@"
}

e2e::log() { printf '\033[1;34m[e2e]\033[0m %s\n' "$*"; }

e2e::fail() {
	printf '\033[1;31m[e2e] FAIL:\033[0m %s\n' "$*" >&2
	exit 1
}

# e2e::wait_for <timeout-seconds> <description> <command...>
# Re-runs the command every 2s until it succeeds or the timeout hits.
e2e::wait_for() {
	local timeout=$1 desc=$2
	shift 2

	local deadline=$((SECONDS + timeout))
	until "$@" >/dev/null 2>&1; do
		if ((SECONDS >= deadline)); then
			e2e::fail "timed out after ${timeout}s waiting for: $desc"
		fi
		sleep 2
	done

	e2e::log "ok: $desc"
}

e2e::daemon_logs() { docker logs "$E2E_DAEMON" 2>&1; }

e2e::daemon_has_log() { e2e::daemon_logs | grep -q "$1"; }

# The failure markers live in the daemon's data volume. `docker cp` works on
# distroless (no shell needed) and errors when the directory does not exist.
e2e::marker_present() {
	docker cp "$E2E_DAEMON":/data/failed-deployments - 2>/dev/null | tar -t 2>/dev/null | grep -q '\.json$'
}

e2e::marker_absent() { ! e2e::marker_present; }

# e2e::container_id <compose-project> <service>
e2e::container_id() {
	docker ps -q --filter "label=com.docker.compose.project=$1" --filter "label=com.docker.compose.service=$2"
}

# e2e::repo_push <commit-message>
# Commits everything currently in $E2E_WORKDIR and pushes to the polled repo.
e2e::repo_push() {
	git -C "$E2E_WORKDIR" add -A
	git -C "$E2E_WORKDIR" commit -qm "$1"
	git -C "$E2E_WORKDIR" push -q origin main
}
