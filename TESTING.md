# TESTING — 测试策略与质量门禁

rucoder-neo 是一个多服务 Rust 项目（12 业务服务 + core SDK）。本文档定义**每个
服务如何保证正确性**，以及 CI 的强制门槛。

## 目录

1. [测试金字塔](#1-测试金字塔)
2. [CI 强制门禁](#2-ci-强制门禁)
3. [分服务的测试要求](#3-分服务的测试要求)
4. [真实调用的分层策略](#4-真实调用的分层策略)
5. [如何新增一个测试](#5-如何新增一个测试)

---

## 1. 测试金字塔

从下往上数量递减、真实性递增：

```
        ▲ 真实 E2E（全链路，极少，独立可选跑）
       ▲▲ 集成测试（服务间，少量，真实下游或 testcontainers）
      ▲▲▲ 契约/组件测试（服务对外 API + 按协议，中量，进程内）
     ▲▲▲▲ 单元测试（纯函数/算法/store，大量）
    ▲▲▲▲▲ 静态检查（fmt/clippy/typos/deny，每次提交，海量）
```

**目标比例**：单元 : 契约 : 集成 : E2E ≈ 70 : 20 : 9 : 1。

铁律：**多数 bug 应在底层被抓**，E2E 只兜关键链路，不用于分支覆盖（太慢、太脆）。

## 2. CI 强制门禁

每个 PR 合并前必须通过（参考 NORA 的 CI 门槛）：

```bash
make gate
```

| 门禁 | 命令 | 失败即阻塞 |
|---|---|---|
| 格式化 | `cargo fmt --check` | 是 |
| Lint | `cargo clippy --all-targets -- -D warnings` | 是 |
| 单元/契约/集成 | `cargo test --workspace` | 是 |
| 依赖审计 | `cargo audit` | 是 |
| 许可/禁令 | `cargo deny check` | 是 |
| 覆盖率地板 | `cargo tarpaulin --config tarpaulin.toml`（`fail-under 40`） | 是 |
| 真实 E2E（可选） | `bash run-all.sh`（registry-tests） | 手动/夜间 |

> 各服务仓库自带 `Makefile`（`test`/`lint`/`gate`）与本仓库根一致。

## 3. 分服务的测试要求

### 3.1 静态检查 — 全服务统一

- `cargo fmt --check`、`cargo clippy --all-targets -- -D warnings` 零告警。

### 3.2 纯业务引擎（registry / agent / worker / repo-manager 的核心）

| 层 | 做法 | 样板 |
|---|---|---|
| 单元 | 把协议编解码、store、解析器抽成无副作用纯函数，海量测试 | `registry/src/curation.rs`、`metrics.rs`、`auth.rs` 时间计算 |
| 契约 | 每个 HTTP 端点用进程内 `tower::ServiceExt::oneshot` | `registry/tests/http_it.rs` |
| 存储 | 真实 DB 测试实例（sqlite 临时文件；postgres 用 testcontainers） | `registry/tests/store_it.rs` |

### 3.3 转发型 tool server（browser/repo/sandbox/artifact-tools）

三件事各锁一层：

| 层 | 验证 | 方式 |
|---|---|---|
| manifest 契约 | tool 名 + JSON schema | 直接调 `provider.tools()` |
| NATS 协议 | `tool.call.X` → `tool.result.{id}` | `MockBus` 模拟 agent |
| 下游转发 | 发出的 HTTP method/path/body + 结果渲染 | `MockServer`（本地 mock HTTP）|

样板：`browser-tools/tests/provider_it.rs`。

### 3.4 依赖外部系统的 business 服务（repo-manager / executor / builder）

- **repo-manager**：真实 `jj`/`git` 三项目式测试（A 建仓库 → B 改文件 → C 读回）。
  参考 `~/recoder-neo/packages/server/tests/routes/registry/repository.test.ts`。
- **executor**：真实 k8s 一次性环境（沿用 registry-tests 的「容器实例 + emptyDir」模式）。
- **builder**：真实 buildkit（一次性实例）。

### 3.5 中枢 agent

- **主线 E2E**：mock LLM 输出 → 触发 tool 调用 → NATS → (伪)tool → 结果回填会话。
  agent 的 bug 多为编排层，单测抓不到，必须有这条主线。

## 4. 真实调用的分层策略

「要不要与 browser/registry/repo-manager 真实交互」分两层，遵循**职责分离**：

| 层 | 交互对象 | 验证 | 成本 |
|---|---|---|---|
| **下游 mock** | 本地 HTTP mock（`MockServer`） | tool server **转发正确**：请求路径/参数/结果渲染 | 低，无外部依赖 |
| **真实服务 E2E** | 真实 browser/registry/... | **跨服务关键链路**真正跑通 | 高，放独立可选套件 |

铁律：`browser-tools` 的正确性 = "它转发对了"（下游 mock 测）；`browser` 的正确性
单独测。两者不绑在一起测——否则既脆又无法归责。

## 5. 如何新增一个测试

1. **纯函数** → `#[cfg(test)] mod tests` 放源文件底部。
2. **HTTP 契约** → `tests/http_it.rs`（进程内 oneshot，复用 `rucoder-sdk-test-utils`）。
3. **NATS 协议 / 下游转发** → `tests/provider_it.rs`（`MockBus` + `MockServer`）。
4. **真实 E2E** → 独立套件（registry-tests 模式），标 `#[ignore]` 或独立脚本。

共享工具（`core/rucoder-sdk-test-utils`）：
- `tempdir()` — 临时目录
- `MockBus`（`core/rucoder-sdk-bus::mock`）— 内存 NATS
- `MockServer` — 本地 HTTP 捕获服务器

---

## 各服务测试现状（盘点基准）

| 服务 | 单元 | 契约 | NATS/转发 | 集成/E2E | 结论 |
|---|---|---|---|---|---|
| registry | ✅ 127 | ✅ | n/a | ✅ 14协议ABC | 基准，完整 |
| browser-tools | — | ✅ | ✅ | — | 本轮补齐 |
| repo-tools | — | ✅ | ✅ | ⬜ | 本轮补齐转发 |
| sandbox-tools | — | ✅ | ✅ | ⬜ | 本轮补齐转发 |
| artifact-tools | — | ✅ | ✅ | ⬜ | 本轮补齐转发 |
| memory-tools | ⬜ | 边界 | ⬜ | ⬜ postgres集成 | 待补 |
| repo-manager | 若干 | ⬜ | — | ⬜ jj/git | 待补 |
| agent | 2 | ⬜ | — | ⬜ 主线 | 待补 |
| executor | 1 | ⬜ | — | ⬜ k8s | 待补 |
| builder | 0 | ⬜ | — | ⬜ buildkit | 待补 |
| worker | 3 | ⬜ | — | ⬜ job | 待补 |
| gateway | 0 | ⬜ | — | ⬜ 路由 | 待补 |

✅ 已达标 · ⬜ 缺失 · — 不适用（n/a）
