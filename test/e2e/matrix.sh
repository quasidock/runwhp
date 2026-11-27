#!/bin/sh
# Runs the end-to-end suite in each configuration, one after another, with
# go test's JSON kept for each, and summarises their times. Run it from Git
# Bash, after build.ps1:
#
#	test/e2e/matrix.sh [test binary flags, such as -test.run=Volume]
#
# RUNWHP_E2E_CONFIGS   configurations to run (default "default cpus1 cpus16 wsl")
# RUNWHP_E2E_PARALLEL  how many tests run at once (default 8, the fastest on the 16-thread test machine)
# RUNWHP_E2E_WSL_FLAGS RUNWHP_E2E_RUN_FLAGS for WSL's kernel; without it, wsl
#                      is skipped
# RUNWHP_E2E_TMP       TMP for the tests' stores; nerdctl's tests then run
#                      apart, with the TMP this script was given, as runwhp's
#                      snapshotter does not start on a ReFS root
# RUNWHP_E2E_BASE      a run's JSON to compare each configuration's with
# GO                   the go command (default go)
#
# The other RUNWHP_E2E_* variables, which name Docker's tools, pass through
# to the tests. Results go to out/e2e: <config>.json, and
# <config>-nerdctl.json when nerdctl's tests run apart.
set -u
cd "$(dirname "$0")/../.."
GO=${GO:-go}
configs=${RUNWHP_E2E_CONFIGS:-default cpus1 cpus16 wsl}
parallel=${RUNWHP_E2E_PARALLEL:-8}
out=out/e2e
mkdir -p "$out"

"$GO" test -c -o out/e2e.test.exe ./test/e2e || exit 1
"$GO" build -o out/e2etime.exe ./test/e2e/internal/e2etime || exit 1
export RUNWHP_E2E=1

# suite NAME FLAGS ARGS... runs the test binary into out/e2e/NAME.json.
suite() {
	name=$1 flags=$2
	shift 2
	start=$(date +%s)
	(cd test/e2e && RUNWHP_E2E_RUN_FLAGS="$flags" ../../out/e2e.test.exe \
		-test.v=test2json -test.count=1 -test.timeout=150m \
		-test.parallel="$parallel" "$@" 2>&1
		echo $? >"../../$out/$name.code") |
		"$GO" tool test2json -t -p e2e >"$out/$name.json"
	echo "$name: exit $(cat "$out/$name.code") in $(($(date +%s) - start)) s" >>"$out/summary.txt"
}

: >"$out/summary.txt"
results=
for c in $configs; do
	case $c in
	default) flags= ;;
	cpus1) flags=--cpus=1 ;;
	cpus16) flags=--cpus=16 ;;
	wsl)
		flags=${RUNWHP_E2E_WSL_FLAGS:-}
		if [ -z "$flags" ]; then
			echo "wsl: skipped, RUNWHP_E2E_WSL_FLAGS is not set" | tee -a "$out/summary.txt"
			continue
		fi
		;;
	*)
		echo "unknown configuration $c" >&2
		exit 2
		;;
	esac
	echo "running $c" >&2
	if [ -n "${RUNWHP_E2E_TMP:-}" ]; then
		mkdir -p "$RUNWHP_E2E_TMP"
		TMP=$RUNWHP_E2E_TMP TEMP=$RUNWHP_E2E_TMP suite "$c" "$flags" -test.skip='^TestNerdctl' "$@"
		results="$results $out/$c.json"
		if [ $# -eq 0 ]; then
			suite "$c-nerdctl" "$flags" -test.run='^TestNerdctl'
			results="$results $out/$c-nerdctl.json"
		fi
	else
		suite "$c" "$flags" "$@"
		results="$results $out/$c.json"
	fi
done

cat "$out/summary.txt"
[ -z "$results" ] && exit 0
if [ -n "${RUNWHP_E2E_BASE:-}" ]; then
	out/e2etime.exe -base "$RUNWHP_E2E_BASE" $results
else
	out/e2etime.exe $results
fi
