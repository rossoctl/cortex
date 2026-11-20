#!/bin/sh
# smoke_inference_check.sh — the release smoke tests' end-to-end request check
# (#956, item 3d). Sends one model request through the installed Cortex to a fake
# Anthropic Messages API (smoke_fake_inference.py) and checks that Cortex parsed it
# and recorded its tokens: 12 in and 3 out, the usage the fake reports.
#
# A fake rather than a real provider: it checks what Cortex does with a request,
# which is what a release must not break, and needs no API key, costs nothing and
# cannot be down. Whether Cortex still matches a real provider's current API is a
# different question, for a separate check that does not gate a release.
#
# The result is read from the session API rather than "the unattended capture path"
# #956 names, because that path (#955) does not exist yet.
#
# Usage: smoke_inference_check.sh
# Expects the default local install: the forward proxy on 127.0.0.1:47600 and the
# session API on 127.0.0.1:47601. SMOKE_PROXY and SMOKE_SESSION_API override them.
set -eu

PROXY="${SMOKE_PROXY:-127.0.0.1:47600}"
SESSION_API="${SMOKE_SESSION_API:-127.0.0.1:47601}"
HERE=$(cd "$(dirname "$0")" && pwd)
# The session id, sent in X-Session-Id (one of the local config's
# session.id_headers), so this request is the only one in its session.
SID="release-smoke-$$"

TMP_DIR="$(mktemp -d)"
fake_pid=""
cleanup() {
	# Reaped too, with stderr silenced: otherwise the shell reports the killed job
	# ("Terminated: 15") as if something had gone wrong. Each step tolerates
	# failure, so set -e cannot make this trap replace the script's exit status.
	if [ -n "${fake_pid}" ]; then
		kill "${fake_pid}" 2>/dev/null || true
		wait "${fake_pid}" 2>/dev/null || true
	fi
	rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

python3 -I "${HERE}/smoke_fake_inference.py" 0 >"${TMP_DIR}/port" 2>"${TMP_DIR}/fake.err" &
fake_pid=$!
# Up to 30s: a cold python3 on a fresh Intel macOS runner took longer than 5.
port=""
for _ in $(seq 1 300); do
	port=$(head -n 1 "${TMP_DIR}/port" 2>/dev/null || true)
	[ -n "${port}" ] && break
	sleep 0.1
done
if [ -z "${port}" ]; then
	echo "FAIL: the fake inference server did not start within 30s ($(command -v python3), $(python3 --version 2>&1))" >&2
	cat "${TMP_DIR}/fake.err" >&2
	exit 1
fi

# --noproxy '' because curl obeys NO_PROXY even with -x: a NO_PROXY that names
# 127.0.0.1, common on a developer's machine, would send this past Cortex. The
# session API calls below take --noproxy '*' for the opposite reason: they must
# reach the session API directly, whatever HTTP_PROXY says.
code=$(curl -sS --max-time 30 --noproxy '' -o "${TMP_DIR}/reply" -w '%{http_code}' \
	-x "http://${PROXY}" "http://127.0.0.1:${port}/v1/messages" \
	-H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' \
	-H 'x-api-key: smoke-test' -H "X-Session-Id: ${SID}" \
	-d '{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[{"role":"user","content":"release smoke test"}]}') || code=000
if [ "${code}" != 200 ]; then
	echo "FAIL: the request through Cortex at ${PROXY} got HTTP ${code}" >&2
	cat "${TMP_DIR}/reply" >&2 2>/dev/null || true
	exit 1
fi

# The response row is appended when Cortex is done with the body, which can be just
# after the client has its reply, so poll briefly rather than read once.
got=""
for _ in $(seq 1 20); do
	got=$(curl -fsS --max-time 5 --noproxy '*' "http://${SESSION_API}/v1/sessions/${SID}" 2>/dev/null |
		jq -r '[.events[] | select(.phase == "response" and .inference != null)] | last
		       | if . == null then "" else "\(.inference.model) \(.inference.inputTokens) \(.inference.outputTokens)" end' \
		2>/dev/null || true)
	[ -n "${got}" ] && break
	sleep 0.5
done
if [ "${got}" != "claude-sonnet-4-5 12 3" ]; then
	echo "FAIL: Cortex did not record the request's tokens: want 'claude-sonnet-4-5 12 3' (model, in, out), got '${got:-no parsed response in session ${SID}}'" >&2
	curl -sS --max-time 5 --noproxy '*' "http://${SESSION_API}/v1/sessions/${SID}" >&2 2>/dev/null || true
	exit 1
fi
total=$(curl -fsS --max-time 5 --noproxy '*' "http://${SESSION_API}/v1/sessions" | jq -r --arg id "${SID}" '.sessions[] | select(.id == $id) | .totalTokens')
if [ "${total}" != 15 ]; then
	echo "FAIL: session ${SID} lists totalTokens '${total}', want 15" >&2
	exit 1
fi
echo "OK: Cortex parsed the request and recorded 12 input + 3 output tokens (session ${SID})"
