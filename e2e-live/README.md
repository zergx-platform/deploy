# e2e-live — extension servers 线上 E2E

真实环境的端到端测试：驱动**已部署**的 repo-extension 和 memory-extension，
走真实 NATS（abep 协议）+ 真实 jj-server + 真实 Postgres。无 mock、无 inproc。

## 前置

- 服务已部署到 temp 集群且健康（`/api/v1/health` = 200）
- 本机可直连集群 svc DNS：
  - `rucoder-nats.temp.svc.cluster.local:4222`
  - `rucoder-repo.temp.svc.cluster.local`
  - `rucoder-postgres.temp.svc.cluster.local:5432`
- Go 1.26，artifact GOPROXY 可用

## 运行

```bash
cd e2e-live
GOPROXY=http://rucoder-artifact.temp.svc.cluster.local:80/pkgs/go \
GOSUMDB=off go run .
```

期望输出 `RESULT: 17 passed, 0 failed`。

## 覆盖

| 扩展 | 工具 |
|---|---|
| memory（id=`memory`） | todowrite / history_search / history_range |
| repo（id=`repo`） | write / read / ls / grep / edit / git-log / git-branches / explore / git-show / git-diff / git-blame / delete |

## 测试数据与清理

- memory：临时 session（`e2e-hist-*`）+ todos，测后删除。
- repo：建 `e2e/smoke` 测试仓库，写/改/删文件制造多个 commit 供 git-* 工具验证，
  测后删除 org `e2e`。

## 注意

- repo 工具用 `_org/_repo/_branch` 参数直传（绕过 repo-extension 的 PG 会话映射表），
  聚焦工具转发与 jj-server 真实交互的正确性。
- git-show/git-diff 用 `write` 返回的 change_id 作 rev（jj 的 change id，非 commit id）。
