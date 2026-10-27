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
#   2. After a REAL reboot, did Cortex come up because of lingering — before
#      anyone logged in — and how long after boot?
#   3. If lingering was already on for some unrelated reason, does
#      uninstalling Cortex correctly leave it on rather than reverting
#      someone else's setting?
#
# How check 2 tells lingering apart from a login. This is the hard part, and
# most of the script exists for it.
#
# Cortex runs under the user's own systemd manager (user@<uid>.service), and
# its unit is WantedBy=default.target, so it starts whenever that manager
# does. Two things start the manager:
#
#   - lingering, at boot, with nobody logged in: the property under test;
#   - any login by that user, through pam_systemd.
#
# The script can only inspect the host by logging in to it over SSH, and that
# login starts the manager, and Cortex with it, if lingering has not already.
# So "is Cortex running when we look?" answers yes whether lingering works or
# is broken. No check made at the time of looking can tell the two apart,
# because the login doing the looking may be what started Cortex.
#
# The order of two events can. The script reads back what the host recorded
# when each happened, both on the same since-boot clock:
#
#   - when user@<uid>.service and cortex.service became active. systemd keeps
#     this per unit (ActiveEnterTimestampMonotonic), and reading it later does
#     not change it.
#   - when this user's first login of the boot opened. logind writes a "New
#     session" line to the journal for every session it opens. The sessions
#     themselves are gone by the time we look, since each SSH probe ends within
#     a second, so the journal is the only record of them. That is what the
#     passwordless sudo journalctl is for.
#
# If both units came up well before the boot's first login, no login can have
# started them, so lingering did. If lingering is broken, nothing starts at
# boot: the first login starts them, and they come up just after it.
#
# Three things keep that comparison honest:
#
#   - The quiet window. sshd comes up at about the moment lingering does, so a
#     login the instant SSH answers leaves no gap to measure. Each cycle stays
#     off the host for QUIET_SECONDS after it goes down, so the boot's first
#     login lands long after lingering's work. Do not SSH to the host yourself
#     during a run: your login would become that boot's first.
#   - Manager sessions are not logins. From systemd 256 on, starting a user
#     manager also opens a logind session for the manager itself (class
#     'manager'), logged with a "New session" line like any login. When
#     lingering works, that line is the user's first of the boot, written as
#     lingering starts the manager; counted as a login, it would fail every
#     good run. So the first login is the first "New session" line that is not
#     a manager session. How the two are told apart depends on the systemd
#     version; collect_boot_evidence explains it.
#   - The margin. The two times are written by different processes, and when a
#     login does start the manager they land about 0.1s apart (the manager
#     went active 0.08s after the login on systemd 257, 0.11s on 261). A margin
#     instead of bare ordering keeps the verdict off which write landed first.
#
# Usage: verify-reboot-lingering.sh <ssh-host> [reboot-count]
#   ssh-host:     user@host, passed straight to ssh. Needs SSH key auth (no
#                 password prompts) and passwordless sudo for reboot,
#                 loginctl and journalctl.
#   reboot-count: how many reboot cycles to run (default 5).
#
# Environment:
#   QUIET_SECONDS  seconds to stay off the host after it goes down (default 30)
#   MIN_MARGIN_S   seconds by which both units must precede the boot's first
#                  login (default 5)
#
# Mutates real state on the target host: installs/uninstalls Cortex, flips
# lingering on and off, reboots it reboot-count times. Point this at a
# disposable box, never anything shared. Expects to start from a clean
# Linger=no baseline (Phase 1 fails loudly, rather than silently adapting
# scope, if it doesn't — that precondition is what makes Phase 1's assertion
# meaningful). A failed run leaves the host as the failure found it, and
# prints the commands that restore that baseline.
set -eu

HOST="${1:?usage: verify-reboot-lingering.sh <ssh-host> [reboot-count]}"
REBOOT_COUNT="${2:-5}"
QUIET_SECONDS="${QUIET_SECONDS:-30}"
MIN_MARGIN_S="${MIN_MARGIN_S:-5}"

# Checked before anything touches the host. A count of 0 or "five" would
# otherwise run no cycles: a failing test in a while condition does not trip
# set -e, so the script would go on to report a pass it never tested.
case "${REBOOT_COUNT}" in
'' | *[!0-9]* | 0*)
	echo "reboot-count must be a positive integer (got '${REBOOT_COUNT}')" >&2
	exit 2
	;;
esac
case "${QUIET_SECONDS}" in
'' | *[!0-9]*)
	echo "QUIET_SECONDS must be a whole number of seconds (got '${QUIET_SECONDS}')" >&2
	exit 2
	;;
esac
case "${MIN_MARGIN_S}" in
'' | *[!0-9]* | 0*)
	echo "MIN_MARGIN_S must be a positive whole number of seconds (got '${MIN_MARGIN_S}')" >&2
	exit 2
	;;
esac

# BatchMode=yes: never fall back to a password prompt — a hang here means
# key auth isn't set up, not "wait for the user to type a password that will
# never come." StrictHostKeyChecking=accept-new: a fresh VM's host key is
# expected to be unknown on first run; a CHANGED key later should still fail
# (accept-new, not off).
#
# ServerAlive*: ConnectTimeout bounds only the TCP connect. A reboot takes the
# host away without closing anything, so a probe that connected just before
# it would otherwise block in read() indefinitely — this once wedged
# wait_down for 11 minutes. sshd itself answers the keepalives, so they do not
# cut a long silent command such as the install.
SSH_BASE="ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=2 -o ServerAliveCountMax=2 -o ConnectionAttempts=1"

REMOTE_USER=""
HOST_CHANGED=0
TMP_DIR="$(mktemp -d)"
CORTEX_FILE="${TMP_DIR}/cortex_active_seconds"
PROBE_FILE="${TMP_DIR}/probe_answered_seconds"
: >"${CORTEX_FILE}"
: >"${PROBE_FILE}"

# Deliberately does not revert the host: after a failure, its state is the
# evidence. It says how to get back to the baseline the next run needs.
on_exit() {
	status="$1"
	rm -rf "${TMP_DIR}"
	if [ "${status}" -ne 0 ] && [ "${HOST_CHANGED}" = 1 ]; then
		{
			echo
			echo "${HOST} was left as the failure found it. To restore the Linger=no baseline:"
			printf "  ssh %s '%s'\n" "${HOST}" '$HOME/.local/bin/agentop service uninstall --yes'
			printf '  ssh %s sudo loginctl disable-linger %s\n' "${HOST}" "${REMOTE_USER}"
		} >&2
	fi
}
trap 'on_exit $?' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

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
	*"Cortex is healthy according to"*) ;;
	*)
		echo "FAIL: agentop service status did not report healthy on ${HOST}" >&2
		echo "${out}" >&2
		exit 1
		;;
	esac
}

# serviceUninstall prints "Removed." on exactly the path that returns 0
# (cmd/agentop/cmd_service.go), so today the two checks agree. Both stay, so a
# change that ever separates them fails here rather than passing.
uninstall_remote() {
	if ! out="$(${SSH_BASE} -o ConnectTimeout=10 "${HOST}" '"$HOME/.local/bin/agentop" service uninstall --yes' 2>&1)"; then
		echo "FAIL: agentop service uninstall exited non-zero on ${HOST}" >&2
		echo "${out}" >&2
		exit 1
	fi
	case "${out}" in
	*Removed.*) ;;
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

remote_boot_id() {
	${SSH_BASE} -o ConnectTimeout=10 "${HOST}" 'cat /proc/sys/kernel/random/boot_id'
}

# Polls for the host to stop, then start, answering SSH — confirms a real
# down/up transition rather than racing a transient blip (down) or declaring
# victory on the first reachable packet before sshd is actually back (up).
# Deadlines are wall-clock rather than iteration counts, so they hold however
# long a single probe takes. The first probe wait_up gets through is a login:
# normally the first of the new boot, which is what the quiet window delays.
wait_down() {
	deadline=$(($(date +%s) + 60))
	while ${SSH_BASE} -o ConnectTimeout=2 "${HOST}" true 2>/dev/null; do
		if [ "$(date +%s)" -ge "${deadline}" ]; then
			echo "FAIL: ${HOST} did not go down within 60s of issuing reboot" >&2
			exit 1
		fi
		sleep 1
	done
}

wait_up() {
	deadline=$(($(date +%s) + 180))
	until ${SSH_BASE} -o ConnectTimeout=3 "${HOST}" true 2>/dev/null; do
		if [ "$(date +%s)" -ge "${deadline}" ]; then
			echo "FAIL: ${HOST} did not come back up within 180s" >&2
			exit 1
		fi
		sleep 1
	done
}

# Runs on the remote host after each reboot. Written as its own script body
# (heredoc with a quoted delimiter, so nothing in it is expanded by THIS
# script) and piped over ssh via stdin, the same way install.sh is piped —
# avoids both the local/remote $VAR quoting hazard and leaving a stray file
# on the host.
#
# Reports what the host recorded; judges nothing. Every time it reports is in
# seconds or microseconds since this boot, on the monotonic clock, so they
# compare directly with each other and need no wall-clock correlation with the
# host running this script.
collect_boot_evidence() {
	cat <<'REMOTE_SCRIPT'
set -eu
uid=$(id -u)
name=$(id -un)
echo "SUBJECT_UID=${uid}"
echo "BOOT_ID=$(cat /proc/sys/kernel/random/boot_id)"
echo "LINGER=$(loginctl show-user "${name}" --property=Linger --value 2>/dev/null)"

# The boot's first login by this user: the earliest "New session" line for
# them that is not the user manager's own session. The header says why it
# comes from the journal. The message changed format in systemd 258, so the
# two are told apart in two ways:
#
#   - 258 and later: New session '2' of user 'alan' with class 'user' and ...
#     The class is in the line, so manager sessions are dropped by its text.
#   - 257 and earlier: New session 2 of user alan.
#     No class. 256 and 257 already open manager sessions, so for each line
#     left, ask logind for that session's class by its number (a logind
#     session number, not a process ID). The manager's session stays open as
#     long as the manager runs, which with lingering is the whole boot, so it
#     answers 'manager' (or 'manager-early') and is skipped. A login counts
#     either way: it answers 'user' while still open, and nothing once it has
#     ended, because logind forgets ended sessions. Session numbers are not
#     reused within a boot, so an ended login's number cannot answer for
#     another session. Before 256 there are no manager sessions to skip.
#
# A manager that restarts mid-boot ends its boot-time session, which then
# answers nothing and counts as a login. The check then fails rather than
# passes. The username is matched both quoted and bare, as fixed strings.
first_login_s=""
if logind="$(sudo -n journalctl -b -u systemd-logind -o short-monotonic --no-pager 2>&1)"; then
	first_login_s="$(printf '%s\n' "${logind}" |
		grep -F 'New session' |
		grep -F -e "of user '${name}'" -e "of user ${name}." |
		grep -v -F "with class 'manager" |
		while IFS= read -r line; do
			sid="$(printf '%s\n' "${line}" | sed -n "s/.*New session '\{0,1\}\([^' ]*\)'\{0,1\} of user .*/\1/p")"
			# </dev/null: loginctl must not read the lines this loop is reading.
			case "$(loginctl show-session "${sid}" -p Class --value 2>/dev/null </dev/null)" in
			(manager*) continue ;;
			esac
			printf '%s\n' "${line}" | sed -n 's/^\[[[:space:]]*\([0-9][0-9]*\.[0-9]*\)\].*/\1/p'
			break
		done)"
	if [ -z "${first_login_s}" ]; then
		printf '%s\n' "${logind}" | grep -F 'New session' | tail -n 5 | sed 's/^/DIAG: /'
	fi
else
	printf '%s\n' "${logind}" | tail -n 3 | sed 's/^/DIAG: journalctl: /'
fi
echo "FIRST_LOGIN_S=${first_login_s}"

echo "USER_MGR_US=$(systemctl show "user@${uid}.service" -p ActiveEnterTimestampMonotonic --value 2>/dev/null)"

# This SSH session is a login, so the bus is up whether or not lingering
# brought it up; the wait only covers it still settling.
rt="/run/user/${uid}"
i=0
while [ "$i" -lt 30 ] && [ ! -S "${rt}/bus" ]; do
	i=$((i + 1))
	sleep 1
done
export XDG_RUNTIME_DIR="${rt}"

# Polled rather than read once: had lingering failed, this session's own login
# would be starting Cortex now, and waiting for it turns "never active" into
# the more telling "active, but only after the first login".
cortex_us=""
i=0
while [ "$i" -lt 30 ]; do
	cortex_us="$(systemctl --user show cortex.service -p ActiveEnterTimestampMonotonic --value 2>/dev/null || true)"
	if [ -n "${cortex_us}" ] && [ "${cortex_us}" != "0" ]; then
		break
	fi
	i=$((i + 1))
	sleep 1
done
echo "CORTEX_US=${cortex_us}"

# cortex.service is Type=simple: going active only proves the process forked.
# The probe proves it answers now, and the moment it first did is an upper
# bound on readiness — it cannot come before this session.
healthz=""
probe_s=""
i=0
while [ "$i" -lt 150 ]; do
	if curl -fs -o /dev/null --max-time 2 http://127.0.0.1:47604/healthz; then
		healthz="ok"
		probe_s="$(awk '{print $1}' /proc/uptime)"
		break
	fi
	i=$((i + 1))
	sleep 0.2
done
echo "HEALTHZ=${healthz}"
echo "PROBE_S=${probe_s}"
REMOTE_SCRIPT
}

# field KEY: the value of KEY=... in the evidence collected for this cycle.
field() { printf '%s\n' "${out}" | sed -n "s/^$1=//p"; }

# check_before_login <unit> <active-us>: fails unless the unit became active at
# least MIN_MARGIN_S before the boot's first login.
check_before_login() {
	unit="$1"
	active_us="$2"
	case "${active_us}" in
	'' | 0 | *[!0-9]*)
		echo "FAIL: cycle ${n}: ${unit} has no activation time in this boot (got '${active_us}')" >&2
		exit 1
		;;
	esac
	if awk -v a="${active_us}" -v l="${first_login_s}" -v m="${MIN_MARGIN_S}" \
		'BEGIN { exit !(l - a / 1000000 >= m) }'; then
		return 0
	fi
	awk -v n="${n}" -v u="${unit}" -v a="${active_us}" -v l="${first_login_s}" -v m="${MIN_MARGIN_S}" \
		'BEGIN { printf "FAIL: cycle %d: %s became active at %.2fs, the first login at %.2fs: a margin of %.2fs, need at least %ds\n", n, u, a / 1000000, l, l - a / 1000000, m }' >&2
	if awk -v a="${active_us}" -v l="${first_login_s}" 'BEGIN { exit !(l - a / 1000000 < 1) }'; then
		echo "  It came up with that login, not ahead of it: lingering most likely did not start it, the login did." >&2
	else
		echo "  It came up before the login, but not by enough to rule the login out. If the first login landed" >&2
		echo "  early in the boot, the quiet window was too short for this host: raise QUIET_SECONDS." >&2
	fi
	exit 1
}

run_cycle() {
	n="$1"
	log "Reboot cycle ${n}/${REBOOT_COUNT}"
	boot_before="$(remote_boot_id)"
	# Allowed to fail: the connection usually drops before reboot returns.
	# Whether the host really rebooted is settled by its boot_id, below.
	${SSH_BASE} -o ConnectTimeout=10 "${HOST}" 'sudo -n reboot' >/dev/null 2>&1 || true
	wait_down
	echo "${HOST} is down; staying off it for ${QUIET_SECONDS}s"
	sleep "${QUIET_SECONDS}"
	wait_up
	out="$(collect_boot_evidence | ${SSH_BASE} -o ConnectTimeout=10 "${HOST}" sh 2>&1)" || {
		echo "FAIL: evidence collection failed on cycle ${n}" >&2
		echo "${out}" >&2
		exit 1
	}

	boot_after="$(field BOOT_ID)"
	if [ -z "${boot_after}" ] || [ "${boot_after}" = "${boot_before}" ]; then
		echo "FAIL: cycle ${n}: boot_id did not change (${boot_before} -> '${boot_after}'): ${HOST} never rebooted" >&2
		exit 1
	fi
	linger="$(field LINGER)"
	if [ "${linger}" != "yes" ]; then
		echo "FAIL: cycle ${n}: lingering is off after the reboot (Linger='${linger}')" >&2
		exit 1
	fi
	first_login_s="$(field FIRST_LOGIN_S)"
	if [ -z "${first_login_s}" ]; then
		echo "FAIL: cycle ${n}: found no login by ${REMOTE_USER} in this boot's journal, though this script just logged in." >&2
		echo "  The match in collect_boot_evidence no longer fits logind's messages. Recent 'New session' lines:" >&2
		printf '%s\n' "${out}" | sed -n 's/^DIAG: /    /p' >&2
		exit 1
	fi
	user_mgr_us="$(field USER_MGR_US)"
	cortex_us="$(field CORTEX_US)"
	check_before_login "user@$(field SUBJECT_UID).service" "${user_mgr_us}"
	check_before_login "cortex.service" "${cortex_us}"
	probe_s="$(field PROBE_S)"
	if [ "$(field HEALTHZ)" != "ok" ] || [ -z "${probe_s}" ]; then
		echo "FAIL: cycle ${n}: cortex.service came up at boot but never answered healthz" >&2
		exit 1
	fi

	awk -v n="${n}" -v m="${user_mgr_us}" -v c="${cortex_us}" -v p="${probe_s}" -v l="${first_login_s}" \
		'BEGIN { printf "cycle %d: manager %.2fs, cortex %.2fs, probe %.2fs | first login %.2fs -> %.2fs margin\n", n, m / 1000000, c / 1000000, p, l, l - (m > c ? m : c) / 1000000 }'
	awk -v c="${cortex_us}" 'BEGIN { printf "%.6f\n", c / 1000000 }' >>"${CORTEX_FILE}"
	echo "${probe_s}" >>"${PROBE_FILE}"
}

# summarize <label> <file>: min/median/max of one figure across the cycles.
summarize() {
	sort -n "$2" | awk -v label="$1" '
		{ a[NR] = $1 }
		END {
			n = NR
			if (n == 0) {
				printf "%s: no cycles recorded\n", label
				exit 1
			}
			if (n % 2 == 1) {
				med = a[(n + 1) / 2]
			} else {
				med = (a[n / 2] + a[n / 2 + 1]) / 2
			}
			printf "%-36s min=%.2fs median=%.2fs max=%.2fs (n=%d)\n", label, a[1], med, a[n], n
		}
	'
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
	'sudo -n reboot --help >/dev/null 2>&1 && sudo -n loginctl --version >/dev/null 2>&1 && sudo -n journalctl --version >/dev/null 2>&1 && echo PRECHECK_OK' 2>&1)" || true
if [ "${precheck_out}" != "PRECHECK_OK" ]; then
	echo "FAIL: passwordless sudo for reboot/loginctl/journalctl is not fully set up on ${HOST}" >&2
	echo "${precheck_out}" >&2
	exit 1
fi
REMOTE_USER="$(${SSH_BASE} -o ConnectTimeout=10 "${HOST}" whoami)"

log "Phase 1: fresh install, assert Cortex claims lingering"
linger_before="$(linger_state)"
if [ "${linger_before}" != "Linger=no" ]; then
	echo "FAIL: ${HOST} must start with lingering off for this assertion to mean anything (got '${linger_before}')." >&2
	echo "Run: ssh ${HOST} sudo loginctl disable-linger ${REMOTE_USER}" >&2
	exit 1
fi
HOST_CHANGED=1
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

log "Phase 2: ${REBOOT_COUNT} reboot cycles (quiet window ${QUIET_SECONDS}s, margin ${MIN_MARGIN_S}s)"
n=1
while [ "$n" -le "${REBOOT_COUNT}" ]; do
	run_cycle "$n"
	n=$((n + 1))
done

# Two figures, because neither alone is "when Cortex was ready". The first is
# systemd's own activation time, the one to quote: Cortex binds its health
# listener a fraction of a second after it, so it is a close lower bound. The
# second is when this script's probe first got an answer, which cannot come
# before sshd or the quiet window — an upper bound that mostly says when the
# script looked. Cortex's own log would not tighten this: its timestamps are
# wall clock, and a host that sets its clock partway through boot moves them
# across reboots.
log "Phase 3: summary (seconds since boot)"
summarize "boot -> cortex.service active" "${CORTEX_FILE}"
summarize "boot -> probe answered (upper bound)" "${PROBE_FILE}"

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
HOST_CHANGED=0

log "Reboot/lingering verification passed for ${HOST}"
