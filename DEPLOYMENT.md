# zergx 上线流程（Deployment Runbook）

> 本文档描述 zergx **多语言（TypeScript / Go / Rust）** 单仓库的完整上线流程：
> 从代码提交 → SDK 发布 → jjlab 同步 → ops-extension 异步构建 → helm 部署 → 验证。
> 所有命令均以当前集群环境为准（namespace `temp`，单节点 hostPath 存储）。

---

## 0. 项目总览：语言与服务清单

| 服务 | 源码目录 | 语言 | 镜像名（`artifact.zergx.10.199.64.20.nip.io/...`） | 角色 |
|---|---|---|---|---|
| agent | `agent-ts/` | TypeScript（Vercel AI SDK） | `zergx-agent-ts:v0.0.1` | 中枢 agent |
| artifact | `artifact/` | Go | `go-registry@<sha256>`（**digest 固定**） | 包/O CI registry（自托管） |
| jjlab | `jjlab/` | Rust | `jjlab:v0.0.1` | 仓库/jj 后端 |
| repo-extension | `repo-extension/` | Go | `jjlab-extension:v0.0.1` | 文件/git 工具层 |
| memory-extension | `memory-extension/` | Go | `zergx-memory-extension:v0.0.1` | 会话记忆 |
| ops-extension | `ops-extension/` | Go（内嵌 Svelte SPA） | `zergx-ops-extension:v0.0.1` | 构建/发布/沙箱工具层 |
| wdbidi-extension | `wdbidi-extension/` | Go | `zergx-wdbidi-extension:v0.0.1` | 浏览器工具（WebDriver BiDi） |
| worker-go | `worker-go/` | Go | `zergx-worker:v0.0.1` | 沙箱 worker（session 内 pod） |
| platform | `platform/` | Go（内嵌 SPA） | `zergx-platform:v0.0.1` | 聚合网关 |
| abc sdk-go | `abc-protocol/sdk-go`（forgejo repo） | Go module | `forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go@v0.x.y` | Go 扩展 SDK（源码在 abc-protocol 单仓） |
| abc sdk-ts | `abc-protocol/sdk-ts`（forgejo repo） | TypeScript | `@abc-protocol/sdk@0.x.y` | TS 扩展 SDK（NATS transport 并入核心包） |

**基础设施镜像**（由 chart 拉取，非本仓库构建）：`postgres:18-alpine3.24`、`nats:2.14.5-alpine`、
`selenium/standalone-chromium:151.0`、`moby/buildkit:v0.32.2-rootless`。

### 关键地址约定

| 项 | 值 |
|---|---|
| Git 远端（统一 forgejo） | `https://forgejo.develop.10.199.64.20.nip.io/zergx/<repo>.git` |
| OCI/包 registry | `artifact.zergx.10.199.64.20.nip.io`（svc 内 `artifact.zergx.svc.cluster.local`） |
| Go module registry（GOPROXY） | `http://artifact.zergx.svc.cluster.local/pkgs/go` |
| npm registry | `http://artifact.zergx.svc.cluster.local/pkgs/npm/` |
| jjlab | `http://jjlab.zergx.svc.cluster.local` |
| ops-extension 构建接口 | `http://ops-extension.zergx.svc.cluster.local/api/v1/images/build` |
| buildkitd | `tcp://zergx-buildkitd.temp.svc.cluster.local:1234` |
| 构建源码事实源 | jjlab `build` org（每 repo 有 `main`/`dev` bookmark） |

---

## 1. 质量门禁（分语言）

提交前必须过对应语言的检查。**没有统一的 `make gate`** —— 各语言各用各的：

### TypeScript 项目（agent-ts）

```bash
# agent-ts（monorepo：schema/agent/server/ui）
cd agent-ts
npm run check            # tsc 类型检查
npx biome check .        # lint + 格式
npx vitest run           # 单元测试（12 文件，55+ 用例）
npm run build            # 完整构建（含 SEA 单二进制）

```

### Go 项目（各 extension / platform / worker；SDK 门禁在 abc-protocol 仓跑）

```bash
cd <service>
go build ./...    # 编译
go vet ./...      # 静态检查
go test ./...     # 单元测试（artifact 有 Makefile：`make check` = fmt+vet+test）
```

### Rust 项目（jjlab）

```bash
cd jjlab
cargo build       # 编译
cargo test        # 单元测试（lib.rs + main.rs）
cargo clippy --all-targets -- -D warnings   # lint（如有配置）
```

### 端到端冒烟（agent 主线）

```bash
# 部署后验证全链路（prompt → NATS → consumer → PG → LLM → 回复）
cd agent-ts
AGENT_BASE=http://zergx-agent.temp.svc.cluster.local bash scripts/smoke.sh
```

---

## 2. 上线总览（一图流）

```
改代码（TS/Go/Rust 各仓库）
   │  1. 本地过门禁 + git commit + push forgejo
   ▼
是否改了 SDK？
   ├─ 是 → 第 3 节：发布 SDK 到 artifact（Go module + npm），
   │        然后下游服务 chore 更新（第 4 节）
   ▼
同步源码到 jjlab build org（第 5 节）
   │  2. DELETE /repos/build/<repo> → clone（带认证 git_url）→ POST /repos/build/<repo>/bookmarks 建 dev
   ▼
ops-extension 异步构建（第 6 节）
   │  3. POST /api/v1/images/build（repo 模式，bookmark=dev，no_cache=true）
   ▼
helm 部署（第 7 节）
   │  4. helm upgrade + rollout restart 对应 deployment
   ▼
验证（第 8 节）
```

---

## 3. SDK 发布到 artifact（核心，必须详细）

两个 SDK 都必须发布到 artifact，**不允许**直接依赖本地路径或 forgejo 源码。

### 3.1 abc sdk-go（Go module → forgejo tag + artifact `/pkgs/go` 镜像）

模块路径 `forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go`，源码在
abc-protocol 单仓（`sdk-go/`），发布为独立 forgejo repo
`abc-protocol/sdk-go`。发布三步：**源码仓门禁 → 同步到发布仓 + git tag →
module zip 上传 artifact 镜像**。

```bash
# 1. 源码仓门禁（/home/user/abc-protocol/sdk-go）
go build ./... && go vet ./... && go test ./...

# 2. 同步到发布仓并打 tag
rm -rf /tmp/sdk-go-publish && cp -r abc-protocol/sdk-go /tmp/sdk-go-publish
cd /tmp/sdk-go-publish && rm -rf .git
git init -q -b main && git add -A
git -c user.name=root -c user.email=root@dev-home.local commit -qm "abc sdk-go v0.1.x"
git remote add origin https://root:<token>@forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go.git
git push -q origin main && git tag v0.1.x && git push -q origin v0.1.x

# 3. 打包 module zip 并 PUT 到 artifact go upload 端点（消费方 GOPRIVATE
#    未覆盖 forgejo 域时走 GOPROXY 镜像；zip 与 tag 缺一不可）
VER=v0.1.x
NAME=forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go
python3 - <<EOF
import io, os, urllib.parse, urllib.request, zipfile
name, ver = "$NAME", "$VER"
base = "http://artifact.zergx.svc.cluster.local"
buf = io.BytesIO()
with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
    for root, dirs, files in os.walk("."):
        dirs[:] = [d for d in dirs if d not in (".git", "node_modules", "target")]
        for f in files:
            p = os.path.join(root, f)
            z.write(p, "%s@%s/%s" % (name, ver, os.path.relpath(p, ".")))
q = urllib.parse.urlencode({"name": name, "version": ver})
req = urllib.request.Request(base + "/pkgs/go/upload?" + q, data=buf.getvalue(), method="PUT")
print(urllib.request.urlopen(req, timeout=30).read().decode())
EOF
# 期望输出：{"ok":true}
```

> **关键点**：tag 必须先推到 forgejo（`go mod download` 用 tag 校验版本）；
> GOPRIVATE=forgejo.develop... 已全局配置时，go 直接走 forgejo git，
> artifact zip 是无 GOPRIVATE 环境（如沙箱构建）的兜底镜像。

### 3.2 abc sdk-ts（npm → forgejo npm registry + artifact 镜像）

包名 `@abc-protocol/sdk`（单包：原 abep-sdk / abep-sdk-nats 合并，NATS
transport 并入核心）。发布两端：forgejo npm registry（权威源）+ artifact
npm 镜像（默认 registry 兜底）。

```bash
cd abc-protocol/sdk-ts/packages/sdk

# 1. 过门禁（单仓内）
npm run check && npm test

# 2. bump 版本（package.json version；npm 不允许覆盖已发布版本）

# 3. 发布到 forgejo npm registry（org abc-protocol；scope 路由已在
#    agent-ts/.npmrc 配好：@abc-protocol:registry=...）
npm publish --access public \
  --registry=https://forgejo.develop.10.199.64.20.nip.io/api/packages/abc-protocol/npm/

# 4. 镜像发布到 artifact npm（无 scope 配置的消费方走默认 registry）
npm publish --access public \
  --registry=http://artifact.zergx.svc.cluster.local/pkgs/npm/
```

> 当前版本：`@abc-protocol/sdk@0.1.0`（TS/Go conformance + interop 双向通过）。

## 4. SDK 更新后，下游服务的 chore 更新

SDK 发布后，所有依赖它的服务必须 bump 版本、重新过门禁、重新构建。

### 4.1 Go 服务（依赖 abc sdk-go：repo-extension / memory-extension / ops-extension / wdbidi-extension）

```bash
for d in repo-extension memory-extension ops-extension wdbidi-extension; do
  cd $d
  # 1. bump go.mod 里的版本
  sed -i 's#abc-protocol/sdk-go v0.1.0#abc-protocol/sdk-go v0.1.1#' go.mod

  # 2. 用 artifact GOPROXY 重新解析 go.sum（必须 GOSUMDB=off，私有模块无公共 sumdb）
  GOPROXY=http://artifact.zergx.svc.cluster.local/pkgs/go \
  GOSUMDB=off GOFLAGS=-mod=mod \
  go mod download forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go

  # 3. 过门禁
  go build ./... && go vet ./... && go test ./...

  # 4. 提交 + push
  git add go.mod go.sum
  git commit -m "chore(deps): bump abc sdk-go"
  git push origin main
  cd ..
done
```

> **注意**：若 `go.mod` 之前残留了本地 `replace` 指令，务必 `go mod edit -dropreplace`
> 再重新下载，否则 CI/构建机拿不到本地路径。

---

## 5. 同步源码到 jjlab build org

构建的事实源是 jjlab 的 `build` org，**不是 forgejo 直接构建**。
每次改完代码 push forgejo 后，必须把 `build` org 对应 repo 重新 clone 到最新 commit。

```bash
REPO=ops-extension   # 改成实际服务目录名（见第 0 节映射）
ORG=build
JJ=http://jjlab.zergx.svc.cluster.local

# 1. 删除旧 repo（连续两次，防 jj init 残留目录竞态）
curl -s -X DELETE "$JJ/api/v1/repos/$ORG/$REPO"
sleep 2
curl -s -X DELETE "$JJ/api/v1/repos/$ORG/$REPO"
sleep 2

# 2. 重新 clone（git_url 必须带认证，否则 self-signed Gitea 会卡在 Username 提示）
curl -s -X POST -H 'Content-Type: application/json' \
  -d "{\"org\":\"$ORG\",\"repo\":\"$REPO\",\"git_url\":\"https://root:devpassword@forgejo.develop.10.199.64.20.nip.io/zergx/$REPO.git\"}" \
  "$JJ/api/v1/repos/clone"
# 期望：{"head":"<commit-sha>","ok":true}

# 3. 建/更新 dev bookmark（构建 tag 后缀 = bookmark 名）
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"rev":"main","branch":"dev"}' \
  "$JJ/api/v1/repos/$ORG/$REPO/bookmarks"
```

> **关键**：
> - 镜像 tag 由 `image_tag` 字段显式指定（版本化发版，`v0.0.x`）；`bookmark` 只决定源码版本（`dev` 始终跟踪 main）。
>   发版走 `release.sh <major|minor|patch> <service...>`，自动 bump 版本 + 构建推送 `{image}:{version}`。
> - clone 后 jj 只保留 forgejo 默认分支（`main`），所以必须再 `POST /repos/{org}/{repo}/bookmarks` 建 `dev`。
> - `git_url` 不带 `root:devpassword@` 会 clone 失败（Gitea 自签证书 + 无凭据）。

---

## 6. ops-extension 异步构建

用 ops-extension 的 `/images/build` 接口（**异步**：立即返回 `build_id`，后台构建）。

```bash
OPS=http://ops-extension.zergx.svc.cluster.local

# repo 模式（标准路径）：从 jjlab 拉源码构建
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{
    "org": "build",
    "repo": "ops-extension",       # jjlab build org 里的 repo 名
    "bookmark": "dev",             # 决定镜像 tag 后缀
    "tag": "zergx-ops-extension",# 镜像名（见第 0 节映射）
    "dockerfile": "Dockerfile",
    "push": true
  }' "$OPS/api/v1/images/build"
# 期望：{"build_id":"...","ok":true}
```

> **构建缓存（重要）**：buildkitd 的 layer cache + content store 持久化在
> hostPath `<hostPathRoot>/buildkit`，跨 pod 重启/helm upgrade 存活。
> **不要默认传 `no_cache:true`**——buildkit 的层缓存按 Dockerfile 指令 +
> 内容哈希自动失效，改源码会精确重建对应层；全局 no-cache 会每次从零重编
> 整个依赖树（jjlab 全量 ≈ 13min，增量 ≈ 30s）。仅在怀疑缓存损坏时
> 显式传 `no_cache:true` 强制重跑。raw 模式（无 repo、纯 Containerfile 文本）
> 服务端已强制 no-cache，前端无需传。

**轮询状态**：

```bash
BID=<build_id>
curl -s "$OPS/api/v1/builds/$BID" | python3 -c \
  'import json,sys; b=json.load(sys.stdin)["build"]; print(b["state"], b.get("error",""))'
# state: running → done / failed
```

**流式看日志**（SSE）：

```bash
curl -sN "$OPS/api/v1/builds/$BID/stream"
```

### 服务 → 构建参数映射（抄这张表）

| 源码目录 | jj build org repo 名 | `tag` 参数（镜像名） |
|---|---|---|
| agent-ts | agent-ts | zergx-agent-ts |
| worker-go | worker-go | zergx-worker |
| jjlab | jjlab | jjlab |
| repo-extension | repo-extension | jjlab-extension |
| memory-extension | memory-extension | zergx-memory-extension |
| ops-extension | ops-extension | zergx-ops-extension |
| wdbidi-extension | wdbidi-extension | zergx-wdbidi-extension |
| platform | platform | zergx-platform |
| artifact（go-registry） | go-registry | go-registry |

> **artifact 特殊**：镜像用 **digest 固定**（`IfNotPresent` + 浮动 tag 会导致 kubelet
> 永不重拉）。重建 go-registry 后，必须：
> 1. 拿到新 digest（`/v2/go-registry/manifests/dev` 的 `Docker-Content-Digest`）；
> 2. 更新 `deploy/charts/zergx/values.yaml` 里 `services.artifact.image` 的 `@sha256:...`；
> 3. `helm upgrade`（见第 7 节）。

### 构建耗时与失败排查

- Go 服务构建快；agent-ts（npm ci + SEA）与 ops-extension（pnpm 前端）较慢。
- 常见失败：
  - `node:22-alpine not found` → registry 只有 `node:26-alpine`，改 Dockerfile。
  - `corepack: not found` → node 26 无 corepack，改 `npm install -g pnpm`。
  - `go mod download ... unknown revision` → 没打 git tag 或没传 artifact（第 3.1 节）。

---

## 6.5 发布政策（强制）：temp 先行，全量绿后才许动 zergx

**zergx namespace 只允许在 temp namespace 完整验证通过后发布。** 无例外。

强制清单（按序执行，任何一步不绿即停）：

1. **SDK 门禁**：`abc-protocol/scripts/ci.sh` 连续两次 GATE PASSED
   （含 `-race`；race 抓过的都是真 bug，不许绕过）。
2. **消费方门禁**：5 服务 + agent-ts 各自测试绿；interop 双向绿。
3. **镜像构建**（构建产物惰性，可先行；但不得触发任何 zergx ns 变更）。
4. **temp 部署 = 升级路径彩排**：`helm --kube-context temp -n temp upgrade zergx-dev ...`
   —— 必须是 **upgrade 而非 uninstall+install**（fresh install 不会踩
   迁移路径；0.2.0 事故正是只在 fresh broker 上验证过）。broker 里若
   存在需迁移的旧状态，保留它并在升级后确认自动迁移。
5. **temp 全量**：e2e-live（ABC_E2E_* 指向 temp）52 项全过 +
   smoke.sh 13 项 + durability.sh 4 项。
6. 以上全绿 → `helm --kube-context zergx -n zergx upgrade zergx` →
   rollout 全部 successfully → 生产 e2e + smoke + durability 复跑。
7. 任一环节失败：修完从第 1 步重来；禁止在 zergx 上试错。

历史教训（写死在这里）：
- 0.2.0 发布时 temp 只验证了 fresh install，未彩排带旧流的升级路径，
  生产滚动遭遇 "subjects overlap" 崩溃环，人工删流补救 12 分钟。
  之后 Connect() 内置了自迁移（0.2.1），且本政策要求 upgrade 彩排。

## 7. helm 部署

```bash
cd deploy

# 1. 渲染校验（dry-run）
helm template zergx charts/zergx

# 2. 升级（若现网被 kubectl 手动改过 image/args，会报 conflict）
helm -n temp upgrade zergx charts/zergx

# 3. 幂等复跑（确认无 drift）
helm -n temp upgrade zergx charts/zergx
```

### 若 helm 报 conflict（历史遗留的 kubectl-set/patch 污染）

```
conflict occurred while applying ... with "kubectl-set" using apps/v1: .spec...image
```

处理：把现网值对齐到 chart 期望，再清掉 kubectl 注解，或删 deployment 让 helm 重建：

```bash
# 方式 A：清 last-applied 注解让 helm 接管
kubectl -n temp annotate deployment zergx-artifact \
  kubectl.kubernetes.io/last-applied-configuration-

# 方式 B：直接删 deployment，helm upgrade 会重建
kubectl -n temp delete deployment zergx-artifact
helm -n temp upgrade zergx charts/zergx
```

### 版本化发版 + 滚动升级

镜像固定语义化版本（`v0.0.x`），不再用浮动 `:dev`。升级某服务：

```bash
for d in zergx-agent zergx-worker jjlab jjlab-extension \
         zergx-memory-tools zergx-ops-extension zergx-wdbidi-extension \
         zergx-platform; do
  kubectl -n temp rollout restart deployment/$d
  kubectl -n temp rollout status deployment/$d --timeout=180s
done
```

> **注意 deployment 名与镜像名不同**：memory-extension 的 deployment 叫
> `zergx-memory-tools`（历史命名）。用 `kubectl get deploy` 确认。

---

## 8. 验证

```bash
# 1. 全部 pod Ready
kubectl -n temp get pods -o wide

# 2. 健康检查
for svc in zergx-agent zergx-wdbidi-extension zergx-memory-tools \
           zergx-ops-extension jjlab-extension; do
  curl -s -o /dev/null -w "$svc -> %{http_code}\n" \
    "http://$svc.temp.svc.cluster.local/api/v1/health"
done

# 3. agent 主线冒烟（创建会话 → prompt → 断言 assistant 回复）
cd agent-ts
AGENT_BASE=http://zergx-agent.temp.svc.cluster.local bash scripts/smoke.sh

# 4. registry 镜像 tag 可拉取
curl -s -I -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
  http://artifact.zergx.svc.cluster.local/v2/<镜像名>/manifests/dev | head -1
```

---

## 9. 常见坑（本次上线踩过的）

| 坑 | 表现 | 解法 |
|---|---|---|
| artifact 镜像浮动 tag 不重拉 | 新 go-registry 不生效 | 镜像 digest 固定 + 每次重建更新 chart |
| buildkit 无持久卷 | pod 重启后缓存全丢、每次全量重编 | chart 已挂 hostPath `<root>/buildkit`；勿再依赖 no_cache 防陈旧 |
| 误用 no_cache | 每次从零编依赖树（jjlab ≈13min） | 默认不传 no_cache；仅在怀疑缓存损坏时传 |
| clone 不带认证 | `could not read Username` | `git_url` 用 `root:devpassword@` |
| clone 后无 dev bookmark | 构建推到 `:main` 而非 `:dev` | `POST /repos/{org}/{repo}/bookmarks` 建 `dev` |
| go 私有模块 sumdb 校验 | `unknown revision` / sum 校验失败 | `GOSUMDB=off` + artifact GOPROXY |
| npm 覆盖旧版本 | `cannot publish over previously published` | bump 版本号 |
| npm publish 报 ENEEDAUTH | registry 匿名但 npm 前置登录 | 配 `_authToken` 假令牌 |
| jjlab 数据丢失 | 每次重启 DROP 全部会话 | 已改 additive DDL（勿再引入 DROP） |
| sandbox pod 拉不到 worker | `zergx-worker:v0.0.1 not found` | 必须先构建 worker-go 镜像 |

---

## 10. 快速参考：改一个服务的最小闭环

以 `ops-extension` 改了一行 Go 代码为例：

```bash
cd ops-extension
go build ./... && go vet ./... && go test ./...        # 门禁
git add -A && git commit -m "fix: ..." && git push origin main   # 提交

# 同步 jjlab（第 5 节）
JJ=http://jjlab.zergx.svc.cluster.local
curl -s -X DELETE "$JJ/api/v1/repos/build/ops-extension"; sleep 2
curl -s -X DELETE "$JJ/api/v1/repos/build/ops-extension"; sleep 2
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"org":"build","repo":"ops-extension","git_url":"https://root:devpassword@forgejo.develop.10.199.64.20.nip.io/zergx/ops-extension.git"}' \
  "$JJ/api/v1/repos/clone"
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"rev":"main","branch":"dev"}' \
  "$JJ/api/v1/repos/build/ops-extension/bookmarks"

# 构建（第 6 节）
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"org":"build","repo":"ops-extension","bookmark":"dev","tag":"zergx-ops-extension","dockerfile":"Dockerfile","push":true,"no_cache":true}' \
  http://ops-extension.zergx.svc.cluster.local/api/v1/images/build
# 轮询 build_id 到 done

# 部署（第 7 节）
kubectl -n temp rollout restart deployment/zergx-ops-extension
kubectl -n temp rollout status deployment/zergx-ops-extension --timeout=180s

# 验证（第 8 节）
curl -s -o /dev/null -w '%{http_code}\n' \
  http://ops-extension.zergx.svc.cluster.local/api/v1/health
```
