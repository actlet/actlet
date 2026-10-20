#!/usr/bin/env bash
# The TUI demo: a scripted session of a couple of minutes with fake model
# usage, run against the development gateway.
#
#   demo.sh tui     run the session with the viewer in the foreground
#   demo.sh attach  attach a viewer to the running demo, or start one in
#                   the background and attach to it
#
# ACTLET_DEMO_DIR (default $TMPDIR/actlet-demo) holds the session folder,
# the gateway's recording and the logs; ACTLET_DEMO_ADDR (default
# 127.0.0.1:7499) is the gateway's address.
set -euo pipefail

mode=${1:-}
here=$(cd "$(dirname "$0")" && pwd)
bin=$(cd "$here/../.." && pwd)/bin
dir=${ACTLET_DEMO_DIR:-${TMPDIR:-/tmp}/actlet-demo}
dir=${dir%/}
addr=${ACTLET_DEMO_ADDR:-127.0.0.1:7499}
session=$dir/session
pids=()

cleanup() {
	for pid in "${pids[@]}"; do
		kill "$pid" 2>/dev/null || true
	done
	wait 2>/dev/null || true
}

box_running() {
	"$bin/actlet" attach -session "$session" -dump >/dev/null 2>&1
}

start_gateway() {
	rm -rf "$dir"
	mkdir -p "$session/inputs" "$dir/gateway"
	cp "$here"/inputs/* "$session/inputs/"
	# The gateway takes operator commands on its standard input: a note
	# for the agent arrives half a minute in.
	mkfifo "$dir/operator"
	"$bin/actlet-devgw" -listen "$addr" -token demo -data "$dir/gateway" \
		-task "$here/task.json" -profile "$here/profile.yaml" -model "$here/model.yaml" \
		<"$dir/operator" >"$dir/gateway.log" 2>&1 &
	pids+=($!)
	(sleep 30 && echo "steer Please keep the report short, and quote every figure in EUR.") >"$dir/operator" &
	pids+=($!)
	for _ in $(seq 50); do
		if (echo >"/dev/tcp/${addr%:*}/${addr##*:}") 2>/dev/null; then
			return
		fi
		sleep 0.2
	done
	echo "demo: the gateway didn't start; see $dir/gateway.log" >&2
	exit 1
}

run_env() {
	ACTLET_GATEWAY=$addr ACTLET_TOKEN=demo ACTLET_TOOLS_MANIFEST= "$@"
}

case $mode in
tui)
	if box_running; then
		echo "demo: a demo is already running; use make demo-attach, or stop it first" >&2
		exit 1
	fi
	trap cleanup EXIT
	start_gateway
	code=0
	run_env "$bin/actlet" run -tui -session "$session" || code=$?
	echo
	case $code in
	0) echo "demo: the session completed. The report is $session/output/report.md" ;;
	3) echo "demo: the session paused. Its folder is $session" ;;
	*) echo "demo: the run ended with exit code $code; logs: $session/.actlet/run.log, $dir/gateway.log" ;;
	esac
	echo "demo: replay it with: bin/actlet-cli replay play $dir/gateway/*.actrec"
	;;
attach)
	if ! box_running; then
		echo "demo: no demo running; starting one in the background (make demo-tui shows it in the foreground)"
		trap cleanup EXIT
		start_gateway
		run_env "$bin/actlet" run -session "$session" >"$dir/run.log" 2>&1 &
		pids+=($!)
		for _ in $(seq 50); do
			box_running && break
			sleep 0.2
		done
	fi
	"$bin/actlet" attach -session "$session"
	;;
*)
	echo "usage: $0 tui|attach" >&2
	exit 2
	;;
esac
