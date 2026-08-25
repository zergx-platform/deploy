# rucoder-neo 上线流程（Deployment Runbook）

> 本文档描述 rucoder-neo **多语言（TypeScript / Go / Rust）** 单仓库的完整上线流程：
> 从代码提交 → SDK 发布 → jj-server 同步 → ops-extension 异步构建 → helm 部署 → 验证。
> 所有命令均以当前集群环境为准（namespace `temp`，单节点 hostPath 存储）。

---

## 0. 项目总览：语言与服务清单

| 服务 | 源码目录 | 语言 | 镜像名（`rucoder-artifact.temp.10.199.64.20.nip.io/...`） | 角色 |
|---|---|---|---|---|
| agent | `agent-ts/` | TypeScript（Vercel AI SDK） | `rucoder-agent-ts:dev` | 中枢 agent |
| artifact | `artifact/` | Go | `go-registry@<sha256>`（**digest 固定**） | 包/O CI registry（自托管） |
| jj-server | `jj-server/` | Rust | `rucoder-repo:dev` | 仓库/jj 后端 |
| repo-extension | `repo-extension/` | Go | `rucoder-repo-extension:dev` | 文件/git 工具层 |
| memory-extension | `memory-extension/` | Go | `rucoder-memory-extension:dev` | 会话记忆 |
| ops-extension | `ops-extension/` | Go（内嵌 Svelte SPA） | `rucoder-ops-extension:dev` | 构建/发布/沙箱工具层 |
| browser-extension | `browser-extension/` | TypeScript | `rucoder-browser-extension:dev` | 浏览器工具 |
| worker-go | `worker-go/` | Go | `rucoder-worker:dev` | 沙箱 worker（session 内 pod） |
| gateway-go | `gateway-go/` | Go（内嵌 SPA） | `rucoder-gateway-go:dev` | 聚合网关 |
| extension-sdk-go | `extension-sdk-go/` | Go module | `forgejo.../rucoder/extension-sdk-go@v0.x.y` | Go 扩展 SDK |
| extension-sdk-ts | `extension-sdk-ts/` | TypeScript | `@rucoder-agent/extension-sdk@0.x.y` | TS 扩展 SDK |

**基础设施镜像**（由 chart 拉取，非本仓库构建）：`postgres:18-alpine3.24`、`nats:2.14.5-alpine`、
`browserless:latest`、`moby/buildkit:v0.32.2-rootless`。

### 关键地址约定

| 项 | 值 |
|---|---|
| Git 远端（统一 forgejo） | `https://forgejo.develop.10.199.64.20.nip.io/rucoder/<repo>.git` |
| OCI/包 registry | `rucoder-artifact.temp.10.199.64.20.nip.io`（svc 内 `rucoder-artifact.temp.svc.cluster.local`） |
| Go module registry（GOPROXY） | `http://rucoder-artifact.temp.svc.cluster.local/pkgs/go` |
| npm registry | `http://rucoder-artifact.temp.svc.cluster.local/pkgs/npm/` |
| jj-server | `http://rucoder-repo.temp.svc.cluster.local` |
| ops-extension 构建接口 | `http://rucoder-ops-extension.temp.svc.cluster.local/api/v1/images/build` |
| buildkitd | `tcp://rucoder-buildkitd.temp.svc.cluster.local:1234` |
| 构建源码事实源 | jj-server `build` org（每 repo 有 `master`/`dev` bookmark） |

---

## 1. 质量门禁（分语言）

提交前必须过对应语言的检查。**没有统一的 `make gate`** —— 各语言各用各的：

### TypeScript 项目（agent-ts / browser-extension / extension-sdk-ts）

```bash
# agent-ts（monorepo：schema/agent/server/ui）
cd agent-ts
npm run check            # tsc 类型检查
npx biome check .        # lint + 格式
npx vitest run           # 单元测试（12 文件，55+ 用例）
npm run build            # 完整构建（含 SEA 单二进制）

# browser-extension / extension-sdk-ts
cd browser-extension && npm run build   # tsc --noEmit
cd extension-sdk-ts    && npm run build  # tsc --noEmit
```

### Go 项目（artifact / 各 extension / gateway / worker / extension-sdk-go）

```bash
cd <service>
go build ./...    # 编译
go vet ./...      # 静态检查
go test ./...     # 单元测试（artifact 有 Makefile：`make check` = fmt+vet+test）
```

### Rust 项目（jj-server）

```bash
cd jj-server
cargo build       # 编译
cargo test        # 单元测试（lib.rs + main.rs）
cargo clippy --all-targets -- -D warnings   # lint（如有配置）
```

### 端到端冒烟（agent 主线）

```bash
# 部署后验证全链路（prompt → NATS → consumer → PG → LLM → 回复）
cd agent-ts
AGENT_BASE=http://rucoder-agent.temp.svc.cluster.local bash scripts/smoke.sh
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
同步源码到 jj-server build org（第 5 节）
   │  2. DELETE /repos/build/<repo> → clone（带认证 git_url）→ bookmark-from dev
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

### 3.1 extension-sdk-go（Go module → artifact `/pkgs/go`）

Go module 的“注册表”天然是 git tag，但本项目的消费方统一走 artifact 的 GOPROXY 镜像。
发布分两步：**git tag** + **module zip 上传 artifact**。

```bash
cd extension-sdk-go

# 1. 过门禁
go build ./... && go vet ./... && go test ./...

# 2. 提交 + 打 tag（tag 版本号与 go.mod 消费方一致）
git add -A
git commit -m "feat: ..."
git tag v0.1.8                 # 新版本号（每次递增）
git push origin master
git push origin v0.1.8

# 3. 打包 module zip 并 PUT 到 artifact go upload 端点
VER=v0.1.8
NAME=forgejo.develop.10.199.64.20.nip.io/rucoder/extension-sdk-go
python3 - <<EOF
import io, os, urllib.parse, urllib.request, zipfile
name, ver = "$NAME", "$VER"
base = "http://rucoder-artifact.temp.svc.cluster.local"
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

> **关键点**：tag 必须先推到 forgejo。`go mod download` 会用 tag 校验版本存在性；
> 只传 zip 不打 tag，消费方会报 `invalid version: unknown revision v0.1.8`。

### 3.2 extension-sdk-ts（npm → artifact `/pkgs/npm`）

```bash
cd extension-sdk-ts

# 1. 过门禁
npm run build          # tsc --noEmit

# 2. bump 版本（npm 不允许覆盖已发布版本）
#    package.json: "version": "0.1.1" → "0.1.2"
sed -i 's/"version": "0.1.1"/"version": "0.1.2"/' package.json

# 3. 提交 + push
git add -A && git commit -m "chore: bump 0.1.2" && git push origin master

# 4. 发布到 artifact npm（anonymous：需要一条 _authToken 配置绕过 npm 的 auth 前置检查）
npm config set //rucoder-artifact.temp.svc.cluster.local/pkgs/npm/:_authToken npm-anonymous --location=project
npm publish --registry http://rucoder-artifact.temp.svc.cluster.local/pkgs/npm/

# 5. 清理临时 .npmrc 与 tgz（不要提交）
rm -f .npmrc rucoder-agent-extension-sdk-*.tgz
```

> **注意**：npm 对已存在版本会 `You cannot publish over the previously published
> versions`。每次必须 bump 版本号。
> artifact 的 npm 端点是 anonymous（`authorizeWrite` 在 `auth==nil` 时放行），
> 但 npm CLI 在 GET 404 时会先要求登录，所以需要 `_authToken` 假令牌让它继续。

---

## 4. SDK 更新后，下游服务的 chore 更新

SDK 发布后，所有依赖它的服务必须 bump 版本、重新过门禁、重新构建。

### 4.1 Go 服务（依赖 extension-sdk-go：repo-extension / memory-extension / ops-extension）

```bash
for d in repo-extension memory-extension ops-extension; do
  cd $d
  # 1. bump go.mod 里的版本
  sed -i 's#extension-sdk-go v0.1.7#extension-sdk-go v0.1.8#' go.mod

  # 2. 用 artifact GOPROXY 重新解析 go.sum（必须 GOSUMDB=off，私有模块无公共 sumdb）
  GOPROXY=http://rucoder-artifact.temp.svc.cluster.local/pkgs/go \
  GOSUMDB=off GOFLAGS=-mod=mod \
  go mod download forgejo.develop.10.199.64.20.nip.io/rucoder/extension-sdk-go

  # 3. 过门禁
  go build ./... && go vet ./... && go test ./...

  # 4. 提交 + push
  git add go.mod go.sum
  git commit -m "chore(deps): bump extension-sdk-go v0.1.7 -> v0.1.8"
  git push origin master
  cd ..
done
```

> **注意**：若 `go.mod` 之前残留了本地 `replace` 指令，务必 `go mod edit -dropreplace`
> 再重新下载，否则 CI/构建机拿不到本地路径。

### 4.2 TS 服务（依赖 extension-sdk-ts：browser-extension）

```bash
cd browser-extension

# 1. bump package.json 依赖版本
sed -i 's#"@rucoder-agent/extension-sdk": "\^0.1.1"#"@rucoder-agent/extension-sdk": "^0.1.2"#' package.json

# 2. 更新 package-lock.json（npm install 的 audit 端点可能报错，可直接从 artifact 拿 integrity 手改）
#    方式 A（可用时）：
npm install --ignore-scripts --package-lock-only \
  --registry http://rucoder-artifact.temp.svc.cluster.local/pkgs/npm/
#    方式 B（npm audit 报错时）：从 artifact 元数据取 integrity 手改 lock
curl -s http://rucoder-artifact.temp.svc.cluster.local/pkgs/npm/@rucoder-agent%2fextension-sdk \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["versions"]["0.1.2"]["dist"]["integrity"])'
# 然后把 package-lock.json 里 sdk 条目的 version/resolved/integrity 三处改为新值

# 3. 过门禁
npm run build          # tsc --noEmit

# 4. 提交 + push
git add package.json package-lock.json
git commit -m "chore(deps): bump @rucoder-agent/extension-sdk to ^0.1.2"
git push origin master
```

---

## 5. 同步源码到 jj-server build org

构建的事实源是 jj-server 的 `build` org，**不是 forgejo 直接构建**。
每次改完代码 push forgejo 后，必须把 `build` org 对应 repo 重新 clone 到最新 commit。

```bash
REPO=ops-extension   # 改成实际服务目录名（见第 0 节映射）
ORG=build
JJ=http://rucoder-repo.temp.svc.cluster.local

# 1. 删除旧 repo（连续两次，防 jj init 残留目录竞态）
curl -s -X DELETE "$JJ/api/v1/repos/$ORG/$REPO"
sleep 2
curl -s -X DELETE "$JJ/api/v1/repos/$ORG/$REPO"
sleep 2

# 2. 重新 clone（git_url 必须带认证，否则 self-signed Gitea 会卡在 Username 提示）
curl -s -X POST -H 'Content-Type: application/json' \
  -d "{\"org\":\"$ORG\",\"repo\":\"$REPO\",\"git_url\":\"https://root:devpassword@forgejo.develop.10.199.64.20.nip.io/rucoder/$REPO.git\"}" \
  "$JJ/api/v1/repos/clone"
# 期望：{"head":"<commit-sha>","ok":true}

# 3. 建/更新 dev bookmark（构建 tag 后缀 = bookmark 名）
curl -s -X POST -H 'Content-Type: application/json' \
  -d "{\"org\":\"$ORG\",\"repo\":\"$REPO\",\"source_rev\":\"master\",\"new_branch\":\"dev\"}" \
  "$JJ/api/v1/repos/bookmark-from"
```

> **关键**：
> - 镜像 tag 后缀 = **bookmark 名**。构建用 `dev` → 镜像 tag `:dev`。
> - clone 后 jj 只保留 forgejo 默认分支（`master`），所以必须再 `bookmark-from` 建 `dev`。
> - `git_url` 不带 `root:devpassword@` 会 clone 失败（Gitea 自签证书 + 无凭据）。

---

## 6. ops-extension 异步构建

用 ops-extension 的 `/images/build` 接口（**异步**：立即返回 `build_id`，后台构建）。

```bash
OPS=http://rucoder-ops-extension.temp.svc.cluster.local

# repo 模式（标准路径）：从 jj-server 拉源码构建
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{
    "org": "build",
    "repo": "ops-extension",       # jj-server build org 里的 repo 名
    "bookmark": "dev",             # 决定镜像 tag 后缀
    "tag": "rucoder-ops-extension",# 镜像名（见第 0 节映射）
    "dockerfile": "Dockerfile",
    "push": true,
    "no_cache": true               # 强制重跑，防 buildkit 陈旧层缓存
  }' "$OPS/api/v1/images/build"
# 期望：{"build_id":"...","ok":true}
```

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
| agent-ts | agent-ts | rucoder-agent-ts |
| worker-go | worker-go | rucoder-worker |
| jj-server | jj-server | rucoder-repo |
| repo-extension | repo-extension | rucoder-repo-extension |
| memory-extension | memory-extension | rucoder-memory-extension |
| ops-extension | ops-extension | rucoder-ops-extension |
| browser-extension | browser-extension | rucoder-browser-extension |
| gateway-go | gateway-go | rucoder-gateway-go |
| artifact（go-registry） | go-registry | go-registry |

> **artifact 特殊**：镜像用 **digest 固定**（`IfNotPresent` + 浮动 tag 会导致 kubelet
> 永不重拉）。重建 go-registry 后，必须：
> 1. 拿到新 digest（`/v2/go-registry/manifests/dev` 的 `Docker-Content-Digest`）；
> 2. 更新 `deploy/charts/rucoder/values.yaml` 里 `services.artifact.image` 的 `@sha256:...`；
> 3. `helm upgrade`（见第 7 节）。

### 构建耗时与失败排查

- Go 服务构建快；agent-ts（npm ci + SEA）与 ops-extension（pnpm 前端）较慢。
- 常见失败：
  - `node:22-alpine not found` → registry 只有 `node:26-alpine`，改 Dockerfile。
  - `corepack: not found` → node 26 无 corepack，改 `npm install -g pnpm`。
  - `go mod download ... unknown revision` → 没打 git tag 或没传 artifact（第 3.1 节）。

---

## 7. helm 部署

```bash
cd deploy

# 1. 渲染校验（dry-run）
helm template rucoder charts/rucoder

# 2. 升级（若现网被 kubectl 手动改过 image/args，会报 conflict）
helm -n temp upgrade rucoder charts/rucoder

# 3. 幂等复跑（确认无 drift）
helm -n temp upgrade rucoder charts/rucoder
```

### 若 helm 报 conflict（历史遗留的 kubectl-set/patch 污染）

```
conflict occurred while applying ... with "kubectl-set" using apps/v1: .spec...image
```

处理：把现网值对齐到 chart 期望，再清掉 kubectl 注解，或删 deployment 让 helm 重建：

```bash
# 方式 A：清 last-applied 注解让 helm 接管
kubectl -n temp annotate deployment rucoder-artifact \
  kubectl.kubernetes.io/last-applied-configuration-

# 方式 B：直接删 deployment，helm upgrade 会重建
kubectl -n temp delete deployment rucoder-artifact
helm -n temp upgrade rucoder charts/rucoder
```

### 滚动重启（让 :dev + Always 的服务拉新镜像）

```bash
for d in rucoder-agent rucoder-worker rucoder-repo rucoder-repo-extension \
         rucoder-memory-tools rucoder-ops-extension rucoder-browser-extension \
         rucoder-gateway; do
  kubectl -n temp rollout restart deployment/$d
  kubectl -n temp rollout status deployment/$d --timeout=180s
done
```

> **注意 deployment 名与镜像名不同**：memory-extension 的 deployment 叫
> `rucoder-memory-tools`（历史命名）。用 `kubectl get deploy` 确认。

---

## 8. 验证

```bash
# 1. 全部 pod Ready
kubectl -n temp get pods -o wide

# 2. 健康检查
for svc in rucoder-agent rucoder-browser-extension rucoder-memory-tools \
           rucoder-ops-extension rucoder-repo-extension; do
  curl -s -o /dev/null -w "$svc -> %{http_code}\n" \
    "http://$svc.temp.svc.cluster.local/api/v1/health"
done

# 3. agent 主线冒烟（创建会话 → prompt → 断言 assistant 回复）
cd agent-ts
AGENT_BASE=http://rucoder-agent.temp.svc.cluster.local bash scripts/smoke.sh

# 4. registry 镜像 tag 可拉取
curl -s -I -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
  http://rucoder-artifact.temp.svc.cluster.local/v2/<镜像名>/manifests/dev | head -1
```

---

## 9. 常见坑（本次上线踩过的）

| 坑 | 表现 | 解法 |
|---|---|---|
| artifact 镜像浮动 tag 不重拉 | 新 go-registry 不生效 | 镜像 digest 固定 + 每次重建更新 chart |
| buildkit 陈旧层缓存 | 改了 Dockerfile 但镜像二进制还是旧的 | 构建接口加 `no_cache:true` |
| clone 不带认证 | `could not read Username` | `git_url` 用 `root:devpassword@` |
| clone 后无 dev bookmark | 构建推到 `:master` 而非 `:dev` | `bookmark-from` 建 `dev` |
| go 私有模块 sumdb 校验 | `unknown revision` / sum 校验失败 | `GOSUMDB=off` + artifact GOPROXY |
| npm 覆盖旧版本 | `cannot publish over previously published` | bump 版本号 |
| npm publish 报 ENEEDAUTH | registry 匿名但 npm 前置登录 | 配 `_authToken` 假令牌 |
| jj-server 数据丢失 | 每次重启 DROP 全部会话 | 已改 additive DDL（勿再引入 DROP） |
| sandbox pod 拉不到 worker | `rucoder-worker:dev not found` | 必须先构建 worker-go 镜像 |

---

## 10. 快速参考：改一个服务的最小闭环

以 `ops-extension` 改了一行 Go 代码为例：

```bash
cd ops-extension
go build ./... && go vet ./... && go test ./...        # 门禁
git add -A && git commit -m "fix: ..." && git push origin master   # 提交

# 同步 jj-server（第 5 节）
JJ=http://rucoder-repo.temp.svc.cluster.local
curl -s -X DELETE "$JJ/api/v1/repos/build/ops-extension"; sleep 2
curl -s -X DELETE "$JJ/api/v1/repos/build/ops-extension"; sleep 2
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"org":"build","repo":"ops-extension","git_url":"https://root:devpassword@forgejo.develop.10.199.64.20.nip.io/rucoder/ops-extension.git"}' \
  "$JJ/api/v1/repos/clone"
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"org":"build","repo":"ops-extension","source_rev":"master","new_branch":"dev"}' \
  "$JJ/api/v1/repos/bookmark-from"

# 构建（第 6 节）
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"org":"build","repo":"ops-extension","bookmark":"dev","tag":"rucoder-ops-extension","dockerfile":"Dockerfile","push":true,"no_cache":true}' \
  http://rucoder-ops-extension.temp.svc.cluster.local/api/v1/images/build
# 轮询 build_id 到 done

# 部署（第 7 节）
kubectl -n temp rollout restart deployment/rucoder-ops-extension
kubectl -n temp rollout status deployment/rucoder-ops-extension --timeout=180s

# 验证（第 8 节）
curl -s -o /dev/null -w '%{http_code}\n' \
  http://rucoder-ops-extension.temp.svc.cluster.local/api/v1/health
```
