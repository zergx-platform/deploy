#!/usr/bin/env bash
# Versioned release tooling for zergx services.
#
# Usage: ./release.sh <bump> <service...>
#          bump    = the version bump kind: major | minor | patch
#          service = one or more service names (see SERVICES below) or "all"
#
# What it does, per service:
#   1. bumps the service's version in charts/zergx/values.yaml (vX.Y.Z)
#   2. syncs the source repo's dev bookmark from forgejo (via jjlab)
#   3. builds & pushes {image}:{version} through ops-extension /images/build
#      using image_tag (source stays on dev, image tag is the release)
#   4. keeps the previous image tag as a fallback record
#
# After running it, verify the chart and roll out:
#   helm -n zergx upgrade zergx charts/zergx
#   kubectl -n temp rollout status deployment/<dep> --timeout=180s
#
# The previous versions are kept in the registry so helm upgrades can be
# rolled back (helm rollback + re-pointing the image tag to the older one).
set -euo pipefail

JJSERVER="${JJSERVER:-http://jj-lab.temp.svc.cluster.local:80}"
OPS="${OPS:-http://ops-extension.zergx.svc.cluster.local}"
CHART_VALUES="$(cd "$(dirname "$0")" && pwd)/charts/zergx/values.yaml"
REGISTRY="${REGISTRY:-artifact.temp.svc.cluster.local}"
NAMESPACE="${NAMESPACE:-zergx}"

# service -> (chart key, jj repo, source image name, k8s deployment)
declare -A SERVICES=(
  [platform]="platform|zergx-platform|zergx-platform|platform"
  [flutter]="flutter|zergx-flutter|zergx-flutter|flutter"
  [jjlab]="jjlab|jjlab|jjlab|jjlab"
  [repo-extension]="repo-extension|repo-extension|zergx-repo-extension|zergx-repo-extension"
  [ops-extension]="ops-extension|ops-extension|zergx-ops-extension|zergx-ops-extension"
  [wdbidi-extension]="wdbidi-extension|wdbidi-extension|zergx-wdbidi-extension|zergx-wdbidi-extension"
  [agent]="agent|zergx-agent|zergx-agent|agent"
  [memory]="memory-tools|memory-extension|zergx-memory-extension|zergx-memory-tools"
  [worker]="worker|zergx-worker|zergx-worker|worker"
)

bump="${1:?usage: release.sh <major|minor|patch> <service...|all>}"
shift
case "$bump" in major|minor|patch) ;; *) echo "bad bump kind: $bump" >&2; exit 2;; esac

sel=("$@")
if [[ "${sel[*]}" == "all" ]]; then
  sel=("${!SERVICES[@]}")
fi
for s in "${sel[@]}"; do
  [[ -n "${SERVICES[$s]:-}" ]] || { echo "unknown service: $s" >&2; exit 2; }
done

# ---- version helpers ----
current_version() { grep -Eo "zergx[-a-z]+:v[0-9.]+" "$CHART_VALUES" | grep "^$1:" | cut -d: -f2; }
bump_version() { # vX.Y.Z kind -> vX'.Y'.Z'
  local v="$1" k="$2" M m p
  M="${v#v}"
  case "$k" in
    major) M="$(( ${M%%.*} + 1 )).0.0";;
    minor) m="${M#*.}"; M="$(( ${M%%.*} )).$(( ${m%%.*} + 1 )).0";;
    patch) m="${M#*.}"; p="${M##*.}"; M="${M%%.*}.${m%%.*}.$(( p + 1 ))";;
  esac
  echo "v$M"
}

for s in "${sel[@]}"; do
  IFS='|' read -r key repo image deployment <<< "${SERVICES[$s]}"

  old="$(grep -E "image: .*${image}:" "$CHART_VALUES" | grep -oE "${image}:v[0-9.]+" | head -1)"
  if [[ -z "$old" ]]; then
    version="v0.0.1"
    oldtag="dev"
  else
    oldv="${old##*:}"
    version="$(bump_version "$oldv" "$bump")"
  fi

  echo "==> $s: ${old:-none} -> ${version}"

  # 1. bump chart (keep the previous version in a comment for easy rollback)
  python3 - "$CHART_VALUES" "$key" "$image" "$oldv" "$version" <<'PY'
import re, sys
path, key, image, oldv, version, host = sys.argv[1:7]
s = open(path).read()
newline = f"    image: {host}/{image}:{version}"
endpoint = f"    image: {host}/{image}:{oldv}" if oldv else None
if endpoint and endpoint in s:
    s = s.replace(endpoint, newline, 1)
else:
    # fall back to any :vX.Y.Z tag for this image
    pat = re.compile(rf"(    image: {host}/{re.escape(image)}:)v[0-9.]+")
    s, n = pat.subn(lambda m: m.group(1) + version, s, count=1)
    if n == 0:
        sys.exit(f"image line not found for {image}")
open(path, 'w').write(s)
PY

  # 2. Force-refresh the build org's copy of the forgejo repo: DELETE the
  #    existing repo (idempotent) then clone, so the version bump ALWAYS builds
  #    today's master — a stale cached clone must never ship a bumped version
  #    carrying old code. jjlab's clone returns 409 CONFLICT for an existing
  #    repo, which `curl -f` would turn into a silent no-op (the prior bug).
  # New jjlab API: repo create/delete then /clone with {url, branch}.
  curl -sf -X DELETE "$JJSERVER/api/v1/repos/build/$repo" >/dev/null 2>&1 || true
  curl -sf -X POST -H 'Content-Type: application/json' \
    -d "{\"default_branch\":\"master\"}" \
    "$JJSERVER/api/v1/repos/build/$repo" >/dev/null 2>&1 || true
  curl -sf -X POST -H 'Content-Type: application/json' \
    -d "{\"url\":\"https://root:devpassword@forgejo.develop.10.199.64.20.nip.io/zergx/$repo.git\",\"branch\":\"master\"}" \
    "$JJSERVER/api/v1/repos/build/$repo/clone" >/dev/null
  # Move master -> dev bookmark (new API: POST /branches/{name} {target}).
  devsha="$(curl -sf "$JJSERVER/api/v1/repos/build/$repo/branches" | python3 -c 'import sys,json; print(json.load(sys.stdin)["branches"][0]["sha"])')"
  curl -sf -X POST -H 'Content-Type: application/json' \
    -d "{\"target\":\"$devsha\"}" \
    "$JJSERVER/api/v1/repos/build/$repo/branches/dev" >/dev/null

  # 3. build + push {image}:{version}
  bid="$(curl -sf -X POST -H 'Content-Type: application/json' \
    -d "{\"org\":\"build\",\"repo\":\"$repo\",\"bookmark\":\"dev\",\"tag\":\"$image\",\"image_tag\":\"$version\",\"dockerfile\":\"Dockerfile\",\"push\":true}" \
    "$OPS/api/v1/images/build" | python3 -c 'import json,sys; print(json.load(sys.stdin)["build_id"])')"

  echo "    build_id=$bid — waiting…"
  for _ in $(seq 1 120); do
    st="$(curl -s --max-time 10 "$OPS/api/v1/builds/$bid" | python3 -c 'import json,sys; print(json.load(sys.stdin)["build"]["state"])' 2>/dev/null || true)"
    case "$st" in
      done)  echo "    ✓ $image:$version built & pushed"; break;;
      failed) echo "    ✗ build failed:"; curl -s "$OPS/api/v1/builds/$bid" | python3 -c 'import json,sys; print(json.load(sys.stdin)["build"].get("error",""))' >&2; exit 1;;
    esac
    sleep 10
  done
done

echo
echo "Done. Roll out with:"
echo "  helm -n $NAMESPACE upgrade zergx charts/zergx"
for s in "${sel[@]}"; do
  IFS='|' read -r _ _ _ deployment <<< "${SERVICES[$s]}"
  echo "  kubectl -n $NAMESPACE rollout status deployment/$deployment --timeout=180s"
done
