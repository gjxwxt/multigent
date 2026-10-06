# 偏差与事实冲突记录（夜间执行，B/S 批）

> 本文件按铁律要求记录"与文档冲突的事实"。只记录，不改文档结论；裁决权在用户。

## DEV-1 secretbox 上游行 161，不是 v3 所述 6 行残留

- **文档口径**：v3 §3「secretbox 先清 6 行残留」。
- **实测（HEAD d8210e81，逐行 porcelain blame）**：internal/secretbox 上游 161 行
  = secretbox.go 103 行 chenhg5（sha 498b18a9，2026-07-16 "fix: allow admins across workspaces and seal model keys"）
  + secretbox_test.go 58 行 chenhg5（同 sha）。
- **验证方式**：git blame --porcelain 与 blame-authors 工具双口径一致；sha 级归属确认。
- **影响**：准净土 6 包中 secretbox 的清理工作量从"6 行"升为"161 行（一个函数级的重写/替换）"，仍远小于整包，准净土定性不变。C1.5 排期时按 161 行计。
- **状态**：待用户在 C1.5 裁决时确认（v3 文档本身不改）。

## DEV-2 workflow 上游行 1,737 → 2,705

- **文档口径**：v3 §1/§3 workflow 1,737 上游行全在 store.go/store_test.go。
- **实测**：2,705 行（store.go 1,980 + store_test.go 725），分布结论不变（其余 43 个文件上游为 0），但绝对量 +968（v3 测量后 chenhg5 对 store 层又有提交）。
- **影响**：「存储层替换与清零残留是同一件事」的 v3 §6 结论不变；阶段 2 工作量口径按 2,705 计。
- **状态**：事实性更新，无需裁决。

## DEV-3 imbridge 上游行 1,531 → 4,661（v3 测量后新增）

- **实测**：整包 9,283 行中上游 4,661。v3 测量时整包 6,153 行；此后新增约 3,130 行代码，自有行保持 4,622 不变，新增行全部来自 chenhg5 提交。
- **影响**：v3 §7.1 前置活 1「deploy_card_bridge 单文件搬、整包不随迁」的正确性被再次确认且更加必要。deploy_card_bridge 本身 blame 零上游（v3 §7.1 实测仍然成立）。
- **状态**：事实性更新，无需裁决。

## DEV-4 v3 全仓总数与本次审计不可直接对比

- v3 全仓 324,556 行 @70cfa058 且含 83 个未提交文件（当时按工作树行归属）；本次 427,618 行 @d8210e81（全部已提交，另含 v3 后新增的 docs/skills/work 下资产与 B1 工具链本身）。自有占比 49.2% → 41.8% 主要反映分母扩大（文档/锁文件/生成物），不反映代码所有权恶化。
- **状态**：口径注释，无需裁决。包级三组数字（db/entity/sandbox）零漂移，佐证审计方法一致。

## DEV-5 v3「web/src 117 文件」现为 122 文件

- v3 后前端有新提交；纯自有 36→38 文件（9,272→10,253 行），deploy 前端 12/12 纯自有（v3 §7.1 结论强化）。root 口径冻结后的两套并记数字见 root-authorship-verdict.md §2。
- **状态**：事实性更新，前端 F 批排期时以本轮数字为准。

## DEV-6 端点注册数 475 的口径

- v3 §7.2 称"注册端点 475 个"。本轮 B2 以脚本实测 `mux.HandleFunc|mux.Handle` 调用点（含 publicMux/runtimeMux/runtimeNodeMux/previewMux 及各 handler 文件内的子 mux）与前端调用字面量交叉，实际数字以 B2 底稿 endpoint-crosswalk.md 为准（与 475 的差异属统计口径：Go 1.22 pattern 一行可注册一个端点，但部分注册点在非 server.go 文件）。
- **状态**：以 B2 底稿数字为准，475 不再作为引用口径。
