# e2e-live — extension servers 线上 E2E

真实环境的端到端测试：驱动**已部署**的 repo/memory/ops 三个扩展，
走真实 NATS（abep 协议）+ 真实 jjlab + 真实 Postgres + 真实 k8s sandbox。
无 mock、无 inproc。

## 前置

- 服务已部署到 temp 集群且健康（`/api/v1/health` = 200）
- 本机可直连集群 svc DNS：
  - `zergx-nats.temp.svc.cluster.local:4222`
  - `zergx-repo.temp.svc.cluster.local`（jjlab）
  - `zergx-postgres.temp.svc.cluster.local:5432`
  - `zergx-ops-extension.temp.svc.cluster.local`
- Go 1.26，artifact GOPROXY 可用
- ops 段需要一个已就绪的 workspace sandbox：session 名 `test:dbg1:main`
  （对应 jj 仓库 `test/dbg1` 的 `main` bookmark；确保 `ensureSandbox` 已拉起
  该会话的 worker pod，或首次 `sandbox-*` 调用会自动创建）

## 运行

```bash
cd e2e-live
GOPROXY=http://zergx-artifact.temp.svc.cluster.local:80/pkgs/go \
GOSUMDB=off go run .
```

期望输出 `RESULT: 36 passed, 0 failed`。

## 覆盖

| 扩展 | 工具 |
|---|---|
| memory（id=`memory`） | todowrite / history_search / history_range |
| repo（id=`repo`） | write / read / ls / grep / edit / git-log / git-branches / explore / git-show / git-diff / git-blame / delete |
| ops（id=`ops`） | sandbox-write / sandbox-read / sandbox-edit / sandbox-run / sandbox-job-list / sandbox-job-output / sandbox-port / list-containerfile-templates / list-registry-packages / image-list / helm-list / container-build / container-deploy / helm-install / helm-status / helm-uninstall |

## 测试数据与清理

- memory：临时 session（`e2e-hist-*`）+ todos，测后删除。
- repo：建 `e2e/smoke` 测试仓库，写/改/删文件制造多个 commit 供 git-* 工具验证，
  测后删除 org `e2e`。
- ops：sandbox 文件写在 workspace 内相对路径（`e2e-ops.txt`、`port-e2e.txt`），
  sandbox-port 会把 `port-e2e.txt` 写进 jj 仓库 `test/dbg1/main` 的
  `ported-e2e.txt`（残留无害，属测试仓库）。

## 注意

- repo 工具用 `_org/_repo/_branch` 参数直传（绕过 repo-extension 的 PG 会话映射表），
  聚焦工具转发与 jjlab 真实交互的正确性。
- git-show/git-diff 用 `write` 返回的 change_id 作 rev（jj 的 change id，非 commit id）。
- ops 的 sandbox 工具通过 session 名 `test:dbg1:main` 解析 workspace；sandbox
  文件路径必须是 workspace 内相对路径（绝对路径会被 `path escapes workspace` 拒绝）。
- ops 的重工具用**唯一运行名**（`e2e-ops-<nanotime>` / 唯一 helm release 名），
  测后自动清理 deployment 与 helm release；镜像 tag 用固定名 `e2e-ops-img`（会被
  后续运行覆盖，属测试镜像）。
- 未纳入（重/时序难控）：package-publish（多协议 CLI 构建）、
  sandbox-job-wait / stdin / kill（sandbox-run 同步等待完成，job 总是 done）。
