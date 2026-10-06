# 全仓许可证审计·人类可读摘要（S0.2）

- 基准 commit：d8210e8128e9b398ad17b4b69aa2293c3f6413fb（分支 license-remediation/batch-execution，含 B1 工具链 d8210e81）
- 生成时间：2026-10-06T15:44:40Z UTC，工具：tools/license-audit/blame-authors（porcelain blame 分作者统计）
- 文件：1273 审计 + 31 二进制跳过；总行数 427618
- 自有（gaojiaxiang）：178672（41.8%）；上游（非自有）：248946；上游行>0 的文件 706 个
- 分桶：{"chenhg5": 189103, "chicken": 241, "gaojiaxiang": 178672, "other": 0, "root": 59602, "uncommitted": 0}

## 判据纪律（v3 §3）
1. 污染判据一律是「上游行 > 0」，禁用比例阈值。
2. per-file/per-package 结论不得外推依赖闭包——见 closure-full.json。

## 包级账本（按上游行降序，前 40）

| 包 | 文件 | 总行 | 自有 | chenhg5 | root | chicken | 上游合计 |
|---|---:|---:|---:|---:|---:|---:|---:|
| internal/api | 298 | 136740 | 72817 | 57454 | 6469 | 0 | 63923 |
| web/src | 122 | 66716 | 17707 | 31350 | 17659 | 0 | 49009 |
| internal/playbook | 167 | 38213 | 9 | 38204 | 0 | 0 | 38204 |
| cmd/multigent | 96 | 29026 | 4948 | 10269 | 13758 | 51 | 24078 |
| docs | 93 | 22516 | 12168 | 9794 | 553 | 1 | 10348 |
| web | 13 | 11482 | 1966 | 345 | 9171 | 0 | 9516 |
| internal/db | 62 | 14388 | 7798 | 6590 | 0 | 0 | 6590 |
| internal/runner | 22 | 9629 | 3797 | 4249 | 1554 | 29 | 5832 |
| internal/imbridge | 24 | 9283 | 4622 | 4661 | 0 | 0 | 4661 |
| internal/docs | 4 | 3541 | 0 | 3541 | 0 | 0 | 3541 |
| cmd/mga | 2 | 3539 | 44 | 3495 | 0 | 0 | 3495 |
| internal/store | 13 | 3481 | 58 | 1887 | 1536 | 0 | 3423 |
| internal/taskstore | 10 | 3507 | 649 | 955 | 1903 | 0 | 2858 |
| internal/workflow | 45 | 17749 | 15044 | 2705 | 0 | 0 | 2705 |
| internal/entity | 10 | 2134 | 270 | 652 | 1212 | 0 | 1864 |
| internal/contextpack | 4 | 1661 | 0 | 1661 | 0 | 0 | 1661 |
| internal/builtins | 12 | 1448 | 131 | 1317 | 0 | 0 | 1317 |
| internal/sandbox | 10 | 2904 | 1658 | 846 | 399 | 1 | 1246 |
| internal/connector | 2 | 1276 | 42 | 1234 | 0 | 0 | 1234 |
| internal/telemetry | 7 | 1124 | 97 | 177 | 850 | 0 | 1027 |
| CHANGELOG.md | 1 | 956 | 0 | 570 | 386 | 0 | 956 |
| internal/formatter | 12 | 911 | 0 | 182 | 729 | 0 | 911 |
| internal/daemon | 6 | 838 | 0 | 71 | 722 | 45 | 838 |
| scripts | 6 | 1053 | 363 | 690 | 0 | 0 | 690 |
| internal/rbac | 2 | 680 | 0 | 680 | 0 | 0 | 680 |
| internal/ctxbuild | 3 | 517 | 0 | 309 | 208 | 0 | 517 |
| INSTALL.md | 1 | 500 | 0 | 427 | 73 | 0 | 500 |
| SKILL.md | 1 | 464 | 0 | 11 | 453 | 0 | 464 |
| .github | 2 | 457 | 0 | 434 | 0 | 23 | 457 |
| npm | 5 | 454 | 0 | 169 | 285 | 0 | 454 |
| internal/runenv | 2 | 521 | 108 | 413 | 0 | 0 | 413 |
| internal/appconfig | 2 | 785 | 422 | 363 | 0 | 0 | 363 |
| skills | 10 | 1353 | 1003 | 256 | 94 | 0 | 350 |
| README.md | 1 | 349 | 0 | 322 | 27 | 0 | 349 |
| README.zh-CN.md | 1 | 338 | 0 | 311 | 27 | 0 | 338 |
| internal/runtimecli | 2 | 335 | 5 | 267 | 0 | 63 | 330 |
| internal/agentdir | 2 | 327 | 0 | 327 | 0 | 0 | 327 |
| internal/template | 1 | 325 | 0 | 0 | 325 | 0 | 325 |
| internal/worker | 3 | 313 | 0 | 1 | 312 | 0 | 313 |
| docker | 5 | 552 | 255 | 186 | 83 | 28 | 297 |

## 依赖闭包扫描结论
- 模块包 56 个；上游>0 污染源 40 个；闭包污染（含传染）47 个
- 污染源清单：cmd/mga, cmd/multigent, internal/agentcli, internal/agentdir, internal/api, internal/appconfig, internal/attention, internal/avatar, internal/builtins, internal/connector, internal/contextpack, internal/ctxbuild, internal/daemon, internal/db, internal/entity, internal/errs, internal/formatter, internal/imbridge, internal/imbridge/lark, internal/interaction, internal/playbook, internal/rbac, internal/runenv, internal/runner, internal/runtimeauth, internal/runtimecli, internal/runtimeexec, internal/runtimeguide, internal/sandbox, internal/scaffold, internal/secretbox, internal/store, internal/taskstore, internal/tasktemplate, internal/telemetry, internal/template, internal/worker, internal/workflow, internal/workspace, tools/export-playbooks

## 与 v3 §1 表的漂移对比

| 对象 | v3 (@70cfa058+83 未提交) | 本审计 (HEAD) | 漂移说明 |
|---|---|---|---|
| 全仓 | 324,556 行 / 自有 49.2% | 427,618 行 / 自有 41.8%（178,672） | 总行 +103k：v3 测量时 83 未提交文件按行归属其作者，现全部落库且包含 docs/skills 等非代码资产；方向判定不变 |
| internal/db | 14,388 / 7,798 自有 / 6,590 上游 (45.8%) | 14,388 / 7,798 / 6,590 | 零漂移（数字逐位吻合） |
| internal/entity | 2,134 / 270 自有 / root 1,212 + chenhg5 652 | 2,134 / 270 / root 1,212 + chenhg5 652 | 零漂移 |
| internal/workflow | 17,470 / ~15,418 自有 / 1,737 上游 | 17,749 / 15,044 / 2,705 | 上游 1,737→2,705：差 968 行全部在 store.go(+255) 与 store_test.go(+725)（v3 测量后这些行又叠加了 chenhg5 提交），结论「上游全在存储层」不变且加重 |
| internal/imbridge | 6,153 / 4,622 自有 / 1,531 上游 | 9,283 / 4,622 / 4,661 | 总行 +3,130：v3 后新增 deploy_card_bridge 等自有代码未计入 v3 自有行（4,622 不变），但 chenhg5 上游 1,531→4,661，整包不净土结论加重 |
| internal/sandbox | 2,904 / 1,658 自有 / 1,246 上游 | 2,904 / 1,658 / 1,246 | 零漂移 |
| web/src | 117 文件 / 纯自有 36 (9,272) / 混同 81 | 122 文件 / 纯自有 38 (10,253) / 混同 84 (56,463，其中自有≈7,454) | 文件 +5（B1 工具不在 web；为 v3 后前端提交），纯自有 +2 个文件；deploy 前端 12 文件全部纯自有（v3 §7.1 结论强化） |

## 关键新发现（v3 未记载）

- **internal/secretbox 上游行 161（103 secretbox.go + 58 secretbox_test.go，全部 chenhg5），不是 v3 所述「6 行残留」**——v3 的 6 行说法与逐行 blame 不符，准净土处置中 secretbox 需按 161 行重写/替换口径评估。
- closure-scan 确认准净土 6 包全部因依赖 db/sandbox 被闭包污染（assets/brownfield/changerun/fixturesandbox/previewreceipt → db；brownfield → sandbox；previewreceipt → secretbox），与 v3 §3 门面策略互相印证。
- chicken 仅 241 行（1 commit，2026-07-21 sandbox/runtime 修复），root 59,602 行（含初始导入），两者数量级不改变主结论。

## 附

- 机器可读全量：audit-full.json（逐文件分桶）、closure-full.json（逐包污染链）
- root 口径冻结：root-authorship-verdict.md
- 存证：tag license-audit-baseline + multigent.bundle（见 S0.4）
