# root 作者口径冻结（S0.3）

> 2026-10-06 · 依据 commit `53b26ac4`（唯一 root 提交）逐行取证 · 工具：`git log --all` + `git blame --line-porcelain` 全仓扫描
> 本文档冻结 v3 §1「root 作者口径警告」要求的身份判定，供后续所有审计数字引用。**冻结后不得再用未定口径的数字做分组或排期决策（v3 §11）。**

## 1. 逐 commit 判定

全仓历史（`git log --all --format='%an <%ae>'`）中 root 身份的提交**只有一个**：

| # | SHA | 作者 | 日期 | 主题 | 规模 |
|---|---|---|---|---|---|
| 1 | `53b26ac469cec54d4cd2c9512ea40f079f0357bb` | root \<root@UYQQVGRAEKQKNQP.\> | 2026-07-14 10:59:29 +0800 | Initial Multigent codebase | 256 files, +73,941 |

再对 HEAD 全仓做 `git blame --line-porcelain`，收集所有存活 root 行的 SHA 集合：
**distinct root-authored SHA = 1**，即 `53b26ac4` 本身。

### 判定

**root ≡ 2026-07-14「Initial Multigent codebase」初始导入，不存在后续 VM 提交。**

证据链：
1. `git log --all --author=root` 仅一条（上述 commit），无第二个 root 身份提交；
2. HEAD blame 中携带 root 行的 220 个文件，其 root 行 SHA 全部收敛到 `53b26ac4`；
3. 该 commit 是仓库第一个提交（256 文件一次性导入，含 web/、internal/、cmd/ 全骨架），主机名形态（`UYQQVGRAEKQKNQP.`）与 "Initial codebase" 主题一致，符合"在某台机器上以 root 用户做了仓库初始化导入"的场景。

**所有权归属裁决**：root 行不是 gaojiaxiang 也不是 chenhg5 的笔名——它无法归属到任一已知作者，因此在许可证判据下**一律按非自有（上游）计**。初始导入的内容来源（是否含上游代码）不受本裁决影响：只要不是 gaojiaxiang 亲笔，就是上游。这也是 v3 §1 把 entity 的 root 1,212 行与非自有并列的口径。

## 2. 冻结后的两套口径（并记）

对本轮 S0.2 审计（HEAD `d8210e81`）的 web/src 逐文件数据，按两种口径统计：

| 口径 | web/src 纯自有（上游=0） | web/src >80% 自有 | 备注 |
|---|---|---|---|
| **A. gaojiaxiang-only（严格口径，本审计默认）** | 38 文件 / 10,253 行 | 39 文件 / 12,285 行 | root 计上游；与 v3 §7.3 逐文件处置表同口径 |
| **B. root 计入自有（宽口径）** | 52 文件 / 10,928 行 | 67 文件 / 19,465 行 | root 与初始导入同源；仅作对照，**不得用于排期** |

v3 §1 警告中引用的 1 vs 9 文件是 @70cfa058 时的旧测量；当前 HEAD 的两口径并记如上。**冻结裁决：后续所有分组、平移、排期决策一律使用口径 A**；口径 B 仅在讨论"初始导入内容的来源追溯"时作为参考。

## 3. 全仓 root 行分布要点（HEAD 实测）

- 全仓 root 行合计 **59,602**（占 427,618 总行的 13.9%）；
- 大户：web/package-lock.json 5,328 / web/pnpm-lock.yaml 3,578（锁文件，重写即消）；cmd/multigent/scheduler.go 1,947；ProjectSchedulePage.tsx 1,190；WorkbenchPage.tsx 1,074；internal/entity/types.go 1,017；
- 携带 root 行的文件 220 个（全仓 1,273 文件的 17.3%）。

## 4. 生效范围

- 本裁决由本轮 S0.3 产出，适用于此后所有 license-audit 工具输出与批次验收；
- 若未来出现新的 root 身份提交（工具人/CI 误配置），blame-authors 会将其单独入桶并在此触发重新冻结——`verify-clean` 与 `blame-authors` 的 other 桶监控是哨兵；
- 口径冻结不改变判据纪律：污染判据仍是「上游行 > 0」（口径 A 下 root 行天然是上游）。
