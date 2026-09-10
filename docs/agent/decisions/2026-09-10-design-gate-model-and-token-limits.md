# OpenDesign 模型选型、单次输出 Token 封顶与执行治理

**Decision**

1. 将设计确认闸门（OpenDesign）的默认模型由 `qwen3.8-27b` 永久切换为 `glm-5.3-flash`（`internal/api/od_client.go:odDefaultModel`）。
2. 设计门下发给 OpenDesign 的需求 Prompt 必须使用已澄清的 `approved_requirement` / `requirement_draft` 全文，并强制注入角色边界（UI 原型设计师，仅在 OD 沙箱内产出单页 HTML/Mock 原型，严禁修改业务工程代码或写生产后端）。
3. 设计启动 API (`handleDesignStart`) 具备自愈机制：若 OD 容器内存在无产物的孤儿同名项目（`proj_mg_{taskID}`），先执行删除自愈再创建，杜绝 `UNIQUE constraint failed: projects.id` 冲突。

**Reason**

1. **容器根只读与 Token 硬封顶**：
   OpenDesign 镜像运行时配置为 `--read-only` 根只读文件系统。其内置的 BYOK 适配层（`apps/daemon/dist/runtimes/byok-opencode.js`）硬编码 `DEFAULT_OUTPUT_TOKEN_LIMIT = 16_384`。无法在容器内部动态扩大该上限。
2. **强思考链模型在 OD 中必然截断失败**：
   在注入 OpenDesign 全局设计规范（~5万字符）和详细需求（~4000字符）时，`qwen3.8-27b` 作为一个深度思考模型，在 `<think>` 中会自动构思全部 CSS 规则、色板、组件、状态机等细节（单次思考输出高达 54,000~59,000 字符，耗尽 16,384 tokens），在尚未闭合思考标签调用 `bash` 写出 `index.html` 的瞬间，被上游推理服务返回 `finish_reason: "length"` 掐断，导致 OpenCode 步进终止、产物为 0，触发 `deliverableValidation: "no_artifact"` 致命失败。
3. **模型网关实测对比证实 GLM-5.3-Flash 最佳**：
   - `glm-5.3-flash`：思考链极克制（~90 tokens）直奔设计骨干，单次仅消耗 2,891 tokens 即可通过 `bash` 生成完整、高保真、带响应式与动效的原型，耗时仅 15 秒，Anthropic Messages 协议与工具调用 100% 兼容。
   - `qwen3.6-35b`：单次消耗 1,167 tokens，耗时 9 秒，亦可稳定落盘，版式丰富度略逊于 GLM。
   - `deepseek-v4-flash`：内网网关未对齐工具调用协议（`tool_calls: null`），不可用于 Agent 驱动。

**Cost to reverse**

若未来需要将设计门更换为其他模型，必须在 CCR 或对应网关实测：
1. 思考链输出不得超过 10,000 tokens，预留至少 6,000 tokens 供文件写入工具调用；
2. 必须支持 Anthropic Messages 协议下的标准 Function / Tool Calling 规范；
3. 严禁使用在无产物时会默默耗尽 16k tokens 的未调校思考模型。
