#!/usr/bin/env bash
# Durability checks against the live zergx cluster:
#   1. memory-tools restart: PG todos survive, tool calls keep working,
#      and the session-ids KV cache (JetStream) survives the bounce.
#   2. agent restart with a queued prompt: the durable mailbox redelivers —
#      the turn runs after the pod comes back (at-least-once + lease).
#
# Usage: bash durability.sh   (run from deploy/e2e-live; needs kubectl with
# the zergx context and cluster DNS)
set -uo pipefail

ZAGENT="${ZAGENT:-http://agent.zergx.svc.cluster.local}"
ZMEMORY="${ZMEMORY:-http://memory-tools.zergx.svc.cluster.local}"
CTX="${CTX:-zergx}"
PASS=0; FAIL=0
pass() { echo "    PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "    FAIL: $1"; FAIL=$((FAIL + 1)); }
check() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (got '$2' want '$3')"; fi; }

bounce() { # bounce <deployment>
  kubectl --context "$CTX" -n zergx rollout restart "deployment/$1" >/dev/null
  kubectl --context "$CTX" -n zergx rollout status "deployment/$1" --timeout=240s >/dev/null
}

echo "[durability] 1. memory-tools restart"
SID="dur-$RANDOM$RANDOM"
curl -sf -X POST -H 'Content-Type: application/json' \
  -d '{"session_id":"'"$SID"'","todos":[{"content":"dur-before","status":"pending","priority":"high"}]}' \
  "$ZMEMORY/api/v1/todos" >/dev/null
bounce memory-tools
sleep 3
todos=$(curl -sf "$ZMEMORY/api/v1/todos?session_id=$SID")
echo "$todos" | grep -q 'dur-before' \
  && pass "todos survive memory-tools restart" || fail "todos lost ($todos)"

echo "[durability] 2. agent restart with a queued prompt"
body=""
for _ in $(seq 1 15); do
  body=$(curl -s -m 10 -X POST -H 'Content-Type: application/json' \
    -d "{\"name\":\"$SID\"}" "$ZAGENT/api/v1/sessions")
  echo "$body" | grep -q '"ok":true' && break
  sleep 2
done
echo "$body" | grep -q '"ok":true' && pass "create session" || fail "create session ($body)"
# submit the prompt, then bounce the agent while the turn is queued/running
curl -sf -X POST -H 'Content-Type: application/json' \
  -d '{"prompt":"reply with exactly: DUR-OK"}' \
  "$ZAGENT/api/v1/sessions/$SID/prompt" >/dev/null &
PROMPT_PID=$!
sleep 1
bounce agent
wait $PROMPT_PID 2>/dev/null || true

state="busy"
for _ in $(seq 1 60); do
  state=$(curl -sf "$ZAGENT/api/v1/sessions/$SID/state" | sed -E 's/.*"status":"([a-z]+)".*/\1/')
  [ "$state" = "idle" ] && break
  sleep 2
done
check "turn reaches idle after restart" "$state" "idle"
msgs=$(curl -sf "$ZAGENT/api/v1/sessions/$SID/messages")
echo "$msgs" | grep -qi 'DUR-OK' \
  && pass "queued prompt processed across restart" || fail "prompt lost ($msgs)"

echo "======================================"
echo "RESULT: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
