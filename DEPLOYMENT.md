# zergx 上线流程（Deployment Runbook）

> 本文档描述 zergx **多语言（TypeScript / Go / Rust）** 项目的完整上线流程：
> 从代码提交（GitHub）→ 源仓库同步到 jjlab → jjlab 直连构建镜像 → helm 部署 → 验证。
> 所有命令均以当前集群环境为准（namespace `temp`，单节点 hostPath 存储）。
>
> **当前架构**：源码事实源 = **GitHub**（`github.com/zergx-platform/*` 等）；镜像/包/依赖
> 全走 **jjlab**（`jj-lab.temp.svc.cluster.local`，内网 svc，pull-through 递归拉取公网
> 基础镜像/依赖）；**jjlab 是唯一的构建执行者**（`/api/v1/ops/builds` 直连），
> ops-extension 退化为薄封装。默认分支统一为 **main**（唯一）。

---

## 0. 项目总览：服务清单

| 服务 | GitHub 仓库 | 语言 | 镜像名（jjlab `root/`） | 角色 |
|---|---|---|---|---|
| platform | `zergx-platform/zergx` | Go（内嵌 SPA） | `zergx-platform` | 聚合网关 + 流式反代 |
| agent | `zergx-platform/agent` | TypeScript（Vercel AI SDK）| `zergx-agent` | 中枢 agent |
| worker | `zergx-platform/worker` | Go | `zergx-worker` | 沙箱 worker |
| flutter-app | `zergx-platform/flutter-app` | Flutter Web | `zergx-flutter` | 平台 Web 前端 |
| memory-extension | `zergx-platform/memory-extension` | Go | `zergx-memory-extension` | 会话记忆 / 文件存储 |
| ops-extension | `zergx-platform/ops-extension` | Go（内嵌 Svelte SPA）| `zergx-ops-extension` | 构建/发布/沙箱工具层（薄封装） |
| repo-extension | `zergx-platform/repo-extension` | Go | `zergx-repo-extension` | 文件/git 工具层 |
| wdbidi-extension | `zergx-platform/wdbidi-extension` | Go | `zergx-wdbidi-extension` | 浏览器工具（WebDriver BiDi） |
| jjlab | `jj-lab-platform/jj-lab` | Rust | `jj-lab` | 仓库 + OCI/包 registry + `/ops` 单体 |
| go-shared | `zergx-platform/go-shared` | Go | —（已内联，无独立部署） | 已废弃，子包内联进各服务 |
| abc sdk-go | `abcp-sdk/abc-protocol-go` | Go | — | Go 扩展 SDK（模块 `github.com/abcp-sdk/abc-protocol-go`）|
| abc sdk-ts | `abcp-sdk/abc-protocol-typescript` | TypeScript | — | TS 扩展 SDK（`@abc-protocol/sdk`）|
| e2e | `jj-lab-platform/e2e-test` | Bash | — | jjlab 注册表 e2e 套件 |

**基础设施镜像**（由 chart 拉取）：`postgres:18-alpine3.24`、`nats:2.14.5-alpine`、
`selenium:151.0-20260808`、`buildkit:v0.32.2-rootless`、`go-registry`（digest 固定）。

### 关键地址约定（全部内网 svc，无 nip.io）

| 项 | 值 |
|---|---|
| Git 远端（来源） | `https://github.com/zergx-platform/<repo>.git`（默认分支 `main`）|
| OCI/包 registry（jjlab） | `http://jj-lab.temp.svc.cluster.local:80` |
| OCI 镜像命名 | `jj-lab.temp.svc.cluster.local/root/<镜像名>:<tag>` |
| Go module registry（GOPROXY） | `http://jj-lab.temp.svc.cluster.local/pkgs/go` |
| npm registry | `http://jj-lab.temp.svc.cluster.local/pkgs/npm/` |
| jjlab 直连构建 | `POST http://jj-lab.temp.svc.cluster.local/api/v1/ops/builds` |
| buildkitd | `tcp://buildkitd.zergx.svc.cluster.local:1234` |
| 构建源码事实源 | jjlab 内 **真实 org/repo**（如 `zergx-platform/ops-extension`）+ 分支 `main` |

> **为何没有 `build` org / `dev` bookmark**：jjlab 现在直接克隆到对应 GitHub org/repo
> （`zergx-platform/<repo>`）并把 bookmark 定为 `main`，不再需要中间 `build` org 或
> `dev` 浮动 bookmark。构建请求的 `org`/`repo` 就是真实前缀（如 `zergx-platform` /
> `ops-extension`），`bookmark=main`。

---

## 1. 质量门禁（分语言）

- **TypeScript（agent）**：`npm run check`（tsc）+ `npx biome check .` + `npx vitest run` + `npm run build`。
- **Go（各 extension / platform / worker）**：`go build ./...` + `go vet ./...` + `go test ./...`。
- **Rust（jjlab）**：`cargo build` + `cargo test`。

提交前必须过对应语言门禁；`go build` 用 `GOPROXY=http://jj-lab.temp.svc.cluster.local/pkgs/go`（经 jjlab 递归拉取，不碰公网死域）。

---

## 2. 上线总览（一图流）

```
改代码（各 GitHub repo，main 分支）
   │  1. 本地过门禁 + git commit + git push origin main
   ▼
jjlab 直连构建（第 3 节）
   │  2. clone 到 jjlab 真实 org/repo（branch=main）→ POST /api/v1/ops/builds
   ▼
helm 部署（第 4 节）
   │  3. helm upgrade 对应 chart；kubelet 从 jjlab svc 拉镜像
   ▼
验证（第 5 节）
```

---

## 3. jjlab 直连构建

### 3.1 同步源码到 jjlab（真实 org/repo + main）

```bash
JJ=http://jj-lab.temp.svc.cluster.local
GIT_TOKEN=<GitHub PAT with repo read access>

# 服务 -> 真实 GitHub 仓库
repo=ops-extension           # 服务目录
githrepo=zergx-platform/ops-extension   # 完整 org/repo
o="${githrepo%/*}"; r="${githrepo#*/}"

# 1. 删除旧 repo（幂等；jjlab clone 对已存在返回 409）
curl -s -X DELETE "$JJ/api/v1/repos/$o/$r" -H "Authorization: token devtoken"

# 2. 重新 clone（branch=main 是唯一分支）
curl -s -X POST -H 'Content-Type: application/json' \
  -d "{\"url\":\"https://$GIT_TOKEN@github.com/$githrepo.git\",\"branch\":\"main\"}" \
  "$JJ/api/v1/repos/$o/$r/clone"
# 期望：{"head":"<commit-sha>","ok":true}
```

### 3.2 触发构建（jjlab 直连）

```bash
# repo 模式：org/repo=真实前缀，bookmark=main，image=jjlab svc root 镜像
curl -s -X POST -H 'Content-Type: application/json' -H "Authorization: token devtoken" \
  -d '{
    "org": "zergx-platform",
    "repo": "ops-extension",
    "bookmark": "main",
    "image": "jj-lab.temp.svc.cluster.local/root/zergx-ops-extension",
    "export": "push",
    "dockerfile": "Dockerfile"
  }' "$JJ/api/v1/ops/builds"
# 期望：{"build_id":"...","ok":true}
```

**轮询** `GET /api/v1/ops/tasks/<build_id>`（status: running→done/failed）；
**流式日志（SSE）** `GET /api/v1/ops/tasks/<build_id>/stream`。

> 版本化发版：用 `release.sh <major|minor|patch> <service...>` 自动 bump 镜像 tag 并构建
> 推送 `{image}:{version}`（内部走上述直连逻辑）。tag 用唯一语义版本（如 `v0.0.13`），
> 每次 bump 必须 **递增**，否则 kubelet 因 tag 相同不拉新镜像。

### 3.3 服务映射（复制此表）

| 服务 | GitHub org/repo | `image` 后缀（root/<名字>）|
|---|---|---|
| platform | zergx-platform/zergx | zergx-platform |
| agent | zergx-platform/agent | zergx-agent |
| worker | zergx-platform/worker | zergx-worker |
| flutter-app | zergx-platform/flutter-app | zergx-flutter |
| memory-extension | zergx-platform/memory-extension | zergx-memory-extension |
| ops-extension | zergx-platform/ops-extension | zergx-ops-extension |
| repo-extension | zergx-platform/repo-extension | zergx-repo-extension |
| wdbidi-extension | zergx-platform/wdbidi-extension | zergx-wdbidi-extension |
| jjlab | jj-lab-platform/jj-lab | jj-lab |

> 注意：jjlab 自身是自举服务——它无法从自己拉镜像（鸡生蛋）。滚动更新时
> `maxSurge` 会先起一个临时 pod（旧 pod 保持在线提供 svc 后端），新 pod 就能从
> `jjlab svc` 拉新镜像。若 jjlab 完全宕机，必须用 forgejo nipio 镜像临时 bootstrap。

---

## 4. helm 部署

```bash
cd deploy

# 1. 渲染校验（dry-run）
helm template zergx-dev charts/zergx -f charts/zergx-dev-values.yaml

# 2. 升级（temp）
helm --kube-context temp -n temp upgrade zergx-dev charts/zergx -f charts/zergx-dev-values.yaml

# 3. 滚动覆盖某服务（若 tag 相同需强制重新拉取，改为 Always 或递增 tag）
kubectl -n temp rollout status deployment/<deployment> --timeout=180s
```

> chart `values.yaml` 的镜像引用格式：`jj-lab.temp.svc.cluster.local/root/<img>:<tag>`。
> 全部走内网 svc；kubelet 需通过节点 `registries.yaml` 把 `jj-lab.temp.svc.cluster.local`
> 配置为 insecure http registry（`endpoint: http://...` + `insecure_skip_tls_verify`）。

---

## 5. 验证

```bash
# 1. 全部 pod Ready
kubectl -n temp get pods -o wide

# 2. 健康检查（svc 直连）
for svc in platform agent repo-extension ops-extension wdbidi-extension memory-tools flutter jj-lab; do
  curl -s -o /dev/null -w "$svc -> %{http_code}\n" "http://$svc.temp.svc.cluster.local/api/v1/health"
done

# 3. registry 镜像 tag 可拉取（jjlab /v2）
curl -s -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
  http://jj-lab.temp.svc.cluster.local/v2/root/<镜像名>/manifests/<tag> | head -1
```

---

## 6. 常见坑

| 坑 | 表现 | 解法 |
|---|---|---|
| 镜像 tag 不变不重拉 | 新代码不生效 | tag **递增** 或 imagePullPolicy=Always；bump 后用新 tag |
| jjlab 自举失败 | ImagePullBackOff `connection refused` | 未用 `maxSurge` 或 jjlab 全宕；用 forgejo nipio 镜像临时 bootstrap |
| 构建 `server closed idle connection` | buildkit 复用空闲连接被 hyper 30s 关闭 | jjlab 已改 `header_read_timeout=300s`（v0.3.18）；勿回退 |
| go/依赖拉死域 | `artifact.zergx` / `forgejo` no such host | GOPROXY/registry 统一指 jjlab svc |
| 源码遗留在 `build` org / `dev` bookmark | 构建到旧代码 | 改为真实 org/repo + `main` |
| 默认分支非 main | release.sh/clone 报错 | 全部仓库 main-only（已统一）|

---

## 7. 快速参考：改一个服务的最小闭环（ops-extension 为例）

```bash
cd ops-extension
go build ./... && go vet ./... && go test ./...          # 门禁（GOPROXY=jjlab svc）
git add -A && git commit -m "fix: ..." && git push origin main

# 同步 jjlab（真实 org/repo + main）
JJ=http://jj-lab.temp.svc.cluster.local
curl -s -X DELETE "$JJ/api/v1/repos/zergx-platform/ops-extension" -H "Authorization: token devtoken"
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"url":"https://<GIT_TOKEN>@github.com/zergx-platform/ops-extension.git","branch":"main"}' \
  "$JJ/api/v1/repos/zergx-platform/ops-extension/clone"

# 构建（jjlab 直连，bookmark=main）
curl -s -X POST -H 'Content-Type: application/json' -H "Authorization: token devtoken" \
  -d '{"org":"zergx-platform","repo":"ops-extension","bookmark":"main","image":"jj-lab.temp.svc.cluster.local/root/zergx-ops-extension","export":"push","dockerfile":"Dockerfile"}' \
  "$JJ/api/v1/ops/builds"

# 部署（递增 chart 里的 tag 后 helm upgrade）+ 验证
kubectl -n temp rollout status deployment/ops-extension --timeout=180s
curl -s -o /dev/null -w '%{http_code}\n' http://ops-extension.temp.svc.cluster.local/api/v1/health
```
