<!--
适用仓库：Yorick-Ryu/CLIProxyAPI（唯一维护分支 beiapi-production，Clouvider 与 DMIT 共用）。
base 分支按改动性质选择；不要用上游默认分支或 tag 覆盖维护历史。
提交前先读 AGENTS.md（payload 最终语义边界、translator 限制、超时规则）
与 .github/BEIAPI_VERSIONING.md（版本基线与本地修订计数）。
运维工作区的 Servers/docs/change-record.md 规定提交信息与发布记录结构。

保留所有小节；不适用写「不适用 + 原因」。小型文档 PR 可精简「复现与验收」，
但「实际验证」「兼容与回滚」「Agent 使用声明」必须保留。

不要把未执行的命令写成通过。日志、截图、请求样例、配置片段必须脱敏：
不得出现 management key、API key、账号 JSON、OAuth token、turn-state、
代理凭据、TLS 私钥或真实用户内容。
-->

## 问题与行为变化 / Problem and behavior change

<!-- 用具体场景描述原行为、改后结果和影响对象，不要只复述文件名。 -->

- 触发条件与原行为 / Trigger and previous behavior:
- 修改后行为 / New behavior:
- 本次范围外与未解决 / Out of scope, still unresolved:

## 来源与关联 / Source and links

- Issue / 相关 PR / Related:
- 上游来源 / Upstream origin（同步 router-for-me/CLIProxyAPI 时必填）:
  - 上游 SHA 与链接 / Upstream SHA and link:
  - 本 fork 的适配差异 / Adaptation differences:
  - 是否只移植部分提交 / Partial port:

## 复现与验收 / Reproduction and acceptance

- 基线 / Baseline：分支、镜像 tag 与 digest 或本地 SHA；相关配置字段名（只写字段名，不写值）
- 前置条件与步骤 / Preconditions and steps:
  1.
  2.
- 修复前观察 / Before:
- 修复后观察与验收标准 / After and acceptance criteria:
- 回归用例位置、覆盖分支与未覆盖情况 / Tests and gaps:

<details>
<summary>协议、传输与调度补充 / Protocol, transport and scheduling（相关时填写）</summary>

- 端点与协议 / Endpoint and protocol：`/v1/responses`、`/responses/compact`、`/v1/chat/completions`、`/v1/messages`、WS
- 下游 → 宿主 → 上游 / Downstream → host → upstream：HTTP / WS / ctx_pool / passthrough / http_bridge / plugin
- 账号类型与平台 / Account type：OAuth、API key、setup-token、SIWC、PAT、Agent Identity、插件账号
- 身份与 turn 元数据 / Identity and turn metadata：identity-convergence 档位、session/thread/turn 映射、turn-state 归属
- 压缩与 framing / Compression：请求 zstd、WS permessage-deflate、消息上限
- 错误分型 / Error classification：HTTP 状态、上游 error type/code、是否计入账号健康
- 重试、故障转移与幂等 / Retry, failover, idempotency：请求是否已发出、流是否已开始、是否允许同账号重试
- 冷却与账号状态 / Cooldown and account state：账号级、模型级、并发占用是否释放

</details>

## 实际验证 / Actual verification

<!-- 一行一个实际检查；结果填「通过 / 失败 / 未运行」，未运行必须写原因。
     CI 链接须对应本 PR 当前 head SHA。 -->

| 检查与执行目录 / 命令 | 结果 | 覆盖范围、证据或未运行原因 |
| --- | --- | --- |
| | | |
| | | |

<!-- UI 或管理接口改动补前后截图与操作步骤；检查截图内是否含密钥、邮箱或账号标识。 -->

## 兼容与回滚 / Compatibility and rollback

- 配置 / API / 默认值 / Config, API, defaults:
- 持久化数据 / Persistence（迁移、字段、回退后残留）:
- 凭据、日志与采集 / Credentials, logs, capture:
- 性能与资源 / Performance（声明提升须给出测量条件）:
- 灰度与回滚 / Canary and rollback:
- 残余风险 / Residual risk:

## 发布与部署 / Release and deployment

<!-- 合并、镜像构建、生产部署是三个独立状态，不要合并表述。 -->

- 目标分支与镜像 tag / Branch and image tag:
- 上游基线版本与 SHA / Upstream baseline version and SHA:
- 本地版本 / Fork version：`v<上游版本>-beiapi.<同步完成后的主线提交次数>`
- 更新基线时的同步完成提交 / Integration commit when advancing the baseline:
- 发布版本标签 / Release version tag（仅该标签 push 触发镜像构建，普通分支 push 不构建）:
- 部署后只读验证 / Post-deploy read-only checks:
- 回滚触发条件与方式 / Rollback trigger and method:

## 文档 / Documentation

<!-- 列出因本改动而失效、已同步更新的文档；无则写「不适用 + 原因」。 -->

## Agent 使用声明 / Agent disclosure

- Agent 名称与参与范围 / Agent and scope:
- 模型（完整名称或 ID）/ Model:
- 推理强度 / Reasoning effort（实际值 / 默认 / 工具未提供 / 未知）:

<!-- 纯人工填写「未使用 Agent」。无法获取的字段写「未知」并说明原因，不得猜测。 -->

## 提交前自查 / Checklist

- [ ] base 分支正确，未夹带无关改动。
- [ ] 未改动 `internal/translator/`（该路径的 PR 会被 CI 拒绝）。
- [ ] payload 规则仍是发送前的最终语义边界，未被后续逻辑覆盖。
- [ ] 正文、提交、测试数据与附件不含凭据、turn-state 或真实用户内容。
- [ ] 实际验证已如实填写，未运行项已说明原因。
- [ ] 已同步受影响的文档与配置示例。
- [ ] Agent 使用声明已填写。
