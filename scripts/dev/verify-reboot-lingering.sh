#!/bin/sh
# verify-reboot-lingering.sh — manual verification of Cortex's systemd --user
# lingering behavior against a REAL reboot, over SSH to a real host.
#
# Neither ci.yaml's agentop leg nor release-smoke-linux.yaml can exercise this:
# both call `sudo loginctl enable-linger` themselves BEFORE install.sh ever
# runs, so Cortex's own "turn lingering on, and remember that I did" branch in
# cmd/agentop/cmd_service_platform.go never fires on either workflow — and
# GitHub-hosted runners are ephemeral, so there's no "reboot it and see what
# happens" available there regardless of architecture. This script drives a
# real reboot cycle against a long-lived host (a VM, a spare box) to check:
#
#   1. Starting from lingering OFF, does Cortex actually turn it on and
#      record that it did (the linger-enabled-by-agentop marker)?
#   2. After a REAL reboot, with nobody logged in, does Cortex actually come
#      back up on its own via lingering — and how long does that take? Timed
#      via monotonic clocks on both ends, so there is no cross-host
#      wall-clock skew to account for.
#   3. If lingering was already on for some unrelated reason, does
#      uninstalling Cortex correctly leave it on rather than reverting
#      someone else's setting?
#
# Usage: verify-reboot-lingering.sh <ssh-host> [reboot-count]
#   ssh-host:     user@host, passed straight to ssh/scp. Needs SSH key auth
#                 (no password prompts) and passwordless sudo for reboot,
#                 loginctl, systemctl, journalctl.
#   reboot-count: how many reboot cycles to time (default 5).
#
# Mutates real state on the target host: installs/uninstalls Cortex, flips
# lingering on and off, reboots it reboot-count+1 times. Point this at a
# disposable box, never anything shared. Expects to start from a clean
# Linger=no baseline (Phase 1 fails loudly, rather than silently adapting
# scope, if it doesn't — that precondition is what makes Phase 1's assertion
# meaningful).
set -eu

HOST="${1:?usage: verify-reboot-lingering.sh <ssh-host> [reboot-count]}"
REBOOT_COUNT="${2:-5}"

# BatchMode=yes: never fall back to a password prompt — a hang here means
# key auth isn't set up, not "wait for the user to type a password that will
# never come." StrictHostKeyChecking=accept-new: a fresh VM's host key is
# expected to be unknown on first run; a CHANGED key later should still fail
# (accept-new, not off).
SSH_BASE="ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
RESULTS_FILE="${TMP_DIR}/healthy_seconds"
: >"${RESULTS_FILE}"

log() { printf '\n== %s ==\n' "$1"; }

# Single quotes at every call site that references a remote-side variable
# ($HOME, $uid, ...): the string must reach the remote shell unexpanded, and
# get expanded there, not here.

assert_healthy() {
	out="$(${SSH_BASE} -o ConnectTimeout=10 "${HOST}" '"$HOME/.local/bin/agentop" service status' 2>&1)" || {
		echo "FAIL: agentop service status exited non-zero on ${HOST}" >&2
		echo "${out}" >&2
		exit 1
	}
	case "${out}" in
	*healthy:* | *"Cortex is healthy according to"*) ;;
	*)
		echo "FAIL: agentop service status did not report healthy on ${HOST}" >&2
		echo "${out}" >&2
		exit 1
		;;
	esac
}

uninstall_remote() {
	out="$(${SSH_BASE} -o ConnectTimeout=10 "${HOST}" '"$HOME/.local/bin/agentop" service uninstall --yes' 2>&1)" || true
	case "${out}" in
	*Removed*) ;;
	*)
		echo "FAIL: uninstall did not report success on ${HOST}" >&2
		echo "${out}" >&2
		exit 1
		;;
	esac
}

install_main_latest() {
	tmp="$(mktemp "${TMP_DIR}/install.XXXXXX")"
	if ! curl -fsSL -o "${tmp}" https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.sh; then
		echo "FAIL: could not download install.sh" >&2
		exit 1
	fi
	# Piped via stdin redirection, not a local file argument, for the same
	# reason release_smoke_test_linux.sh does it: install.sh's own re-exec
	# bootstrap only re-fetches itself when $0 isn't a readable file, the
	# real `curl | sh` condition. Here it's piped over ssh instead of to a
	# local sh, but the property is the same.
	${SSH_BASE} -o ConnectTimeout=10 "${HOST}" "sh -s -- --ref=main-latest --yes" <"${tmp}"
}

linger_state() {
	${SSH_BASE} -o ConnectTimeout=10 "${HOST}" "loginctl show-user ${REMOTE_USER} --property=Linger"
}

marker_present() {
	${SSH_BASE} -o ConnectTimeout=10 "${HOST}" '[ -f "$HOME/.cortex/linger-enabled-by-agentop" ]'
}

# Polls for the host to stop, then start, answering SSH — confirms a real
# down/up transition rather than racing a transient blip (down) or declaring
# victory on the first reachable packet before sshd is actually back (up).
wait_down() {
	i=0
	while [ "$i" -lt 60 ]; do
		if ! ${SSH_BASE} -o ConnectTimeout=2 "${HOST}" true 2>/dev/null; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	echo "FAIL: ${HOST} did not go down within 60s of issuing reboot" >&2
	exit 1
}

wait_up() {
	i=0
	while [ "$i" -lt 180 ]; do
		if ${SSH_BASE} -o ConnectTimeout=3 "${HOST}" true 2>/dev/null; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	echo "FAIL: ${HOST} did not come back up within 180s" >&2
	exit 1
}

# Runs on the remote host after each reboot. Written as its own script body
# (heredoc with a quoted delimiter, so nothing in it is expanded by THIS
# script) and piped over ssh via stdin, the same way install.sh is piped —
# avoids both the local/remote $VAR quoting hazard and leaving a stray file
# on the host.
#
# All three readings come off the monotonic clock (systemd's
# ActiveEnterTimestampMonotonic, and /proc/uptime for the healthz probe), so
# they're directly comparable to each other with no wall-clock correlation
# needed between this host and the one running this script.
collect_timing() {
	cat <<'REMOTE_SCRIPT'
set -eu
uid=$(id -u)
rt="/run/user/${uid}"

# Same wait-for-bus-socket recipe ci.yaml's agentop leg and
# release-smoke-linux.yaml both use to give a lingering-kept-alive user
# session time to come up.
i=0
while [ "$i" -lt 60 ]; do
	[ -S "${rt}/bus" ] && break
	i=$((i + 1))
	sleep 1
done
if [ ! -S "${rt}/bus" ]; then
	echo "FAIL_NO_BUS"
	exit 1
fi
export XDG_RUNTIME_DIR="${rt}"

user_mgr_us="$(systemctl show "user@${uid}.service" -p ActiveEnterTimestampMonotonic --value)"

# cortex.service may still be starting even once the bus is up; poll briefly
# rather than assuming it's already active.
cortex_us=""
i=0
while [ "$i" -lt 60 ]; do
	cortex_us="$(systemctl --user show cortex.service -p ActiveEnterTimestampMonotonic --value 2>/dev/null || echo "")"
	if [ -n "${cortex_us}" ] && [ "${cortex_us}" != "0" ]; then
		break
	fi
	i=$((i + 1))
	sleep 1
done
if [ -z "${cortex_us}" ] || [ "${cortex_us}" = "0" ]; then
	echo "FAIL_NO_CORTEX"
	exit 1
fi

# cortex.service is Type=simple: ActiveEnterTimestamp only proves the process
# forked, not that it's answering. Polling healthz and reading /proc/uptime
# the instant it first succeeds is the metric that actually matters.
healthy_s=""
i=0
while [ "$i" -lt 150 ]; do
	if curl -fs -o /dev/null http://127.0.0.1:47604/healthz; then
		healthy_s="$(awk '{print $1}' /proc/uptime)"
		break
	fi
	i=$((i + 1))
	sleep 0.2
done
if [ -z "${healthy_s}" ]; then
	echo "FAIL_NO_HEALTHZ"
	exit 1
fi

echo "USER_MGR_US=${user_mgr_us}"
echo "CORTEX_US=${cortex_us}"
echo "HEALTHY_S=${healthy_s}"
REMOTE_SCRIPT
}

run_cycle() {
	n="$1"
	log "Reboot cycle ${n}/${REBOOT_COUNT}"
	${SSH_BASE} -o ConnectTimeout=10 "${HOST}" 'sudo -n reboot' >/dev/null 2>&1 || true
	wait_down
	wait_up
	out="$(collect_timing | ${SSH_BASE} -o ConnectTimeout=10 "${HOST}" sh 2>&1)" || {
		echo "FAIL: timing collection failed on cycle ${n}" >&2
		echo "${out}" >&2
		exit 1
	}
	case "${out}" in
	*FAIL_NO_BUS*)
		echo "FAIL: cycle ${n}: /run/user/<uid>/bus never appeared — lingering did not bring up a user session" >&2
		exit 1
		;;
	*FAIL_NO_CORTEX*)
		echo "FAIL: cycle ${n}: cortex.service never became active" >&2
		exit 1
		;;
	*FAIL_NO_HEALTHZ*)
		echo "FAIL: cycle ${n}: cortex.service started but never answered healthz" >&2
		exit 1
		;;
	esac
	user_mgr_us="$(printf '%s\n' "${out}" | sed -n 's/^USER_MGR_US=//p')"
	cortex_us="$(printf '%s\n' "${out}" | sed -n 's/^CORTEX_US=//p')"
	healthy_s="$(printf '%s\n' "${out}" | sed -n 's/^HEALTHY_S=//p')"
	if [ -z "${healthy_s}" ]; then
		echo "FAIL: cycle ${n} produced no HEALTHY_S line" >&2
		echo "${out}" >&2
		exit 1
	fi
	awk -v u="${user_mgr_us}" -v c="${cortex_us}" -v h="${healthy_s}" -v n="${n}" \
		'BEGIN { printf "cycle %d (seconds since boot): user-manager %.2f, cortex start %.2f, healthy %.2f\n", n, u / 1000000, c / 1000000, h }'
	echo "${healthy_s}" >>"${RESULTS_FILE}"
}

log "Preconditions"
if ! ${SSH_BASE} -o ConnectTimeout=10 "${HOST}" true; then
	echo "FAIL: cannot reach ${HOST} over SSH with key auth" >&2
	exit 1
fi
# Checked without side effects: --help/--version on each, never the bare
# command — this must never actually trigger a reboot as a side effect of
# checking whether reboot is ALLOWED.
precheck_out="$(${SSH_BASE} -o ConnectTimeout=10 "${HOST}" \
	'sudo -n reboot --help >/dev/null 2>&1 && sudo -n loginctl --version >/dev/null 2>&1 && sudo -n systemctl --version >/dev/null 2>&1 && sudo -n journalctl --version >/dev/null 2>&1 && echo PRECHECK_OK' 2>&1)" || true
if [ "${precheck_out}" != "PRECHECK_OK" ]; then
	echo "FAIL: passwordless sudo for reboot/loginctl/systemctl/journalctl is not fully set up on ${HOST}" >&2
	echo "${precheck_out}" >&2
	exit 1
fi
REMOTE_USER="$(${SSH_BASE} -o ConnectTimeout=10 "${HOST}" whoami)"

log "Phase 1: fresh install, assert Cortex claims lingering"
linger_before="$(linger_state)"
if [ "${linger_before}" != "Linger=no" ]; then
	echo "FAIL: ${HOST} must start with lingering off for this assertion to mean anything (got '${linger_before}')." >&2
	echo "Run: ${SSH_BASE} ${HOST} sudo loginctl disable-linger ${REMOTE_USER}" >&2
	exit 1
fi
install_main_latest
assert_healthy
linger_after="$(linger_state)"
if [ "${linger_after}" != "Linger=yes" ]; then
	echo "FAIL: install did not enable lingering (got '${linger_after}')" >&2
	exit 1
fi
if ! marker_present; then
	echo "FAIL: lingering marker missing after install — Cortex enabled lingering but did not record owning it" >&2
	exit 1
fi
echo "OK: lingering enabled and claimed by Cortex"

log "Phase 2: ${REBOOT_COUNT} reboot-timing cycles"
n=1
while [ "$n" -le "${REBOOT_COUNT}" ]; do
	run_cycle "$n"
	n=$((n + 1))
done

log "Phase 3: summary (boot -> cortex healthy)"
sort -n "${RESULTS_FILE}" -o "${RESULTS_FILE}"
awk '
	{ a[NR] = $1 }
	END {
		n = NR
		min = a[1]
		max = a[n]
		if (n % 2 == 1) {
			med = a[(n + 1) / 2]
		} else {
			med = (a[n / 2] + a[n / 2 + 1]) / 2
		}
		printf "min=%.2fs median=%.2fs max=%.2fs (n=%d)\n", min, med, max, n
	}
' "${RESULTS_FILE}"

log "Phase 4: lingering-ownership edge case"
uninstall_remote
linger="$(linger_state)"
if [ "${linger}" != "Linger=no" ]; then
	echo "FAIL: uninstall did not turn lingering back off after Cortex itself enabled it (got '${linger}')" >&2
	exit 1
fi
echo "OK: uninstall turned off the lingering Cortex itself enabled"

echo "Manually enabling lingering (simulating a pre-existing, unrelated preference)"
${SSH_BASE} -o ConnectTimeout=10 "${HOST}" "sudo -n loginctl enable-linger ${REMOTE_USER}"

install_main_latest
assert_healthy
if marker_present; then
	echo "FAIL: marker file present after installing onto an ALREADY-lingering host — Cortex wrongly claimed ownership of a preference it didn't set" >&2
	exit 1
fi
echo "OK: install onto pre-existing lingering did not claim ownership"

uninstall_remote
linger="$(linger_state)"
if [ "${linger}" != "Linger=yes" ]; then
	echo "FAIL: uninstall reverted a pre-existing lingering preference it did not create (got '${linger}')" >&2
	exit 1
fi
echo "OK: uninstall left a pre-existing lingering preference untouched"

log "Cleanup: restoring ${HOST} to its original Linger=no baseline"
${SSH_BASE} -o ConnectTimeout=10 "${HOST}" "sudo -n loginctl disable-linger ${REMOTE_USER}"

log "Reboot/lingering verification passed for ${HOST}"
