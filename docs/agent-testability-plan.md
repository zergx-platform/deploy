# 立项：Agent 可测性重构 — 主线 E2E 方案

## 1. 目标与背景

**问题**：agent 是系统中枢（会话编排 + LLM 流式推理 + NATS 工具调用 + 结果回填），但
目前**编译后无法离线测试主线**——`run_turn_once` 的三个依赖都无法 mock：

| 依赖 | 当前类型 | 为何无法 mock |
|---|---|---|
| LLM | `LlmClient`（内部 `rig_core` 真 HTTP） | 具体 struct，无 trait |
| 数据库 | `rucoder_sdk_db::Db`（`sqlx::PgPool`） | 具体 struct，硬绑 postgres |
| 事件总线 | `rucoder_sdk_bus::Bus`（真 NATS） | 具体 struct |

**目标**：让 agent 的「主线」——**用户提问 → LLM 返回 tool call → NATS 调工具 →
结果回填会话历史**——能在进程内、离线、确定性跑通。

**成功标准**：一条 E2E 测试，用 mock LLM 返回一个 `browse_navigate`（或 `todowrite`）
tool call，断言最终会话里出现了 `tool_result` part 且内容正确，全程不碰真实
LLM/postgres/NATS。

## 2. 核心方案：抽三个 trait + mock 实现

原则：**只加边界，不改行为**。把 `LlmClient`/`Db`/`Bus` 抽象成 trait，生产用现有
实现，测试注入内存/mock 实现。

### 2.1 `LlmClient` → `Llm` trait（agent 仓库内，`rucoder-llm`）

```rust
// rucoder-llm/src/lib.rs 或新文件
#[async_trait::async_trait]
pub trait Llm: Send + Sync {
    async fn stream(
        &self,
        model: &str,
        messages: &[types::Message],
        tools: &[types::ToolDef],
    ) -> types::Result<Vec<types::StreamEvent>>;
}

impl Llm for LlmClient { ... }   // 现有实现直接满足

/// 测试用：脚本化返回，不联网
pub struct ScriptedLlm { pub events: Vec<types::StreamEvent> }
#[async_trait::async_trait]
impl Llm for ScriptedLlm { ... }
```

- `AppState.llm` 类型从 `Arc<LlmClient>` 改为 `Arc<dyn Llm>`。
- `main.rs` 构造处不变化（`Arc::new(LlmClient::new(...))` 自动 coerce 到 `Arc<dyn Llm>`）。

### 2.2 `Db` → `Db` trait（`core/rucoder-sdk-db`）

问题：`Db` 有 ~40 个方法（sessions/messages/parts/mailbox/presets/config/orgs/repos），
直接抽 trait 会很大。**渐进策略**：

- 先定义 `Db` trait 只含 `run_turn_once` 实际用到的方法子集（见 §4），其余方法
  保持 `Db` struct 原有（不动）。
- 或者更轻：**不抽 `Db` 全量 trait**，而是给 `Db` 加一个 `sqlite` 后端能力
  （`rucoder-sdk-db` 已在 core，可像 registry 那样把 postgres↔sqlite 的 SQL 占位符
  差异收口）→ agent 测试用 `Db::open(sqlite://...)` 临时文件，**零 mock**。

  对比：registry 已经走过「PgPool → Sqlite」这步（`store/mod.rs`、`migrations`），
  `rucoder-sdk-db` 的 SQL 用 `$1` 占位，改 sqlite 需 `?`。**推荐方案**：给
  `rucoder-sdk-db` 加 sqlite 后端（feature 门控），agent 测试用真实 sqlite 临时库，
  比抽 40 方法 trait 更实在、更接近生产。

### 2.3 `Bus` → `BusTrait`

好消息：`BusTrait` **已经存在**（`rucoder-sdk-bus`），且已有 `MockBus`。唯一障碍是
代码里写死了 `Arc<Bus>` 具体类型。改法：

- `AppState.bus`、`discover_tools`、`DiscoveredTool.bus`、`push_event` 里的 `Bus`
  引用，泛化成 `BusTrait`（`Box<dyn BusTrait>` 或泛型参数 `B: BusTrait`）。
- `DiscoveredTool` 已在 `tools.rs`，持有具体 `Bus`；改为持有 `Arc<dyn BusTrait>`。

## 3. 主线 E2E 测试（目标产物）

```rust
// agent/tests/mainline_e2e.rs
#[tokio::test]
async fn tool_call_roundtrip() {
    // 1. sqlite 临时库（或 mock Db）
    // 2. Arc<MockBus>
    // 3. ScriptedLlm：返回一个 StreamEvent::ToolCall{ name:"todowrite", ... }
    // 4. 构造 AppState（注入三者）
    // 5. create_session + prompt("做个 todo")
    // 6. 轮询 db.list_parts(sid) 直到出现 type=="tool_result"
    // 7. 断言 tool_result.content 正确、消息链完整
}
```

## 4. `run_turn_once` 依赖清单（决定 Db trait 最小子集）

从 `main.rs` `run_turn_once` + 相关 handler 提取实际调用的 Db 方法：

- `get_session(sid)`
- `list_messages(sid)`
- `list_parts(sid)`
- `set_session_usage(sid, i, o, t)`
- `insert_message(&MessageRow)`
- `insert_part(&PartRow)`

若走 sqlite 后端方案，则无需 subset——所有方法都可用真实 sqlite。

## 5. 实施步骤（分阶段，每步可独立提交/回滚）

| 步骤 | 内容 | 风险 | 备注 |
|---|---|---|---|
| S1 | `rucoder-llm` 抽 `Llm` trait + `ScriptedLlm` | 低 | 纯增量，生产行为不变 |
| S2 | `tools.rs` 泛化 `BusTrait`（`DiscoveredTool`/`discover_tools`） | 中 | 需改 main.rs 构造点 |
| S3 | `AppState` 三个字段类型改 `Arc<dyn ...>` | 中 | 涉及 `push_event`/`run_session_turn` 签名 |
| S4 | `rucoder-sdk-db` 加 sqlite 后端（或抽 `Db` trait 子集） | 高 | 占位符 `$1`→`?` + migrations |
| S5 | 写 `mainline_e2e.rs` | 低 | 串起 1-4 的产出 |
| S6 | 跑通 + clippy 零告警 + 提交 | 低 | |

## 6. 风险与权衡

- **S4（db）是最大不确定点**：`rucoder-sdk-db` 若改 sqlite，会影响 agent **和**
  repo-manager（两处都用 `rucoder_sdk_db::Db`；repo-manager 已另用 `RepoDb` sqlite，
  agent 仍用 postgres）。需要确认 sqlite 语义（事务、`ON CONFLICT`、`NOW()`）在 S4 中
  是否都可平移。
- **S2/S3 泛型化**：若用 `box<dyn>` 会引入一次动态分发（可忽略）；用泛型参数则会
  「传染」到大量函数签名（噪音大）。**倾向 `Arc<dyn Trait>`**。
- **不改变生产行为**是本重构的硬约束：所有改动只在「注入点」插入 trait 边界，
  运行时仍路由到 `LlmClient`/`Db(postgres)`/`Bus(NATS)`。

## 7. 验收

- [ ] `cargo test -p rucoder-agent` 含至少 1 条主线 E2E（离线、确定性、< 1s）。
- [ ] `cargo clippy --all-targets -- -D warnings` 零告警。
- [ ] `cargo test --workspace`（agent 仓库内）全绿，生产路径未回归。
- [ ] 现有容器/部署（chart）无需任何改动即可升级（镜像接口不变）。

---

## 附：为何不先做「更简单的替代」？

替代方案「只 mock LLM、其余用真实 NATS+postgres 一次性容器」——技术上可行，但：
- 每次测试要起真 NATS + 真 postgres 容器（慢、有状态、CI 重）；
- 仍是「集成测试」而非「主线单测」，抓不到编排层逻辑回归的精确位置；
- 抽 trait 后，同一个 `ScriptedLlm`/`MockBus` 能同时服务单元、契约、主线三层，
  长期收益更高。

故本方案坚持「抽边界、进程内 mock」，把容器集成测试留给 repo-manager/executor
那类必须真环境的服务（已按 TESTING.md 分层处理）。
