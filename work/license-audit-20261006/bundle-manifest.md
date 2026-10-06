# S0.4 存证清单

- 存证时间：2026-10-06T15:47Z（本地）
- 基准分支：license-remediation/batch-execution @ 3261cc8d
- tag：`license-audit-baseline`（指向 3261cc8d）
- bundle：`multigent.bundle`（git bundle create --all，85 refs，完整历史含所有分支与 tag）
- sha256：`b17b36eb0920f44fbf106d45a8bca3617f66269be1a3a6d8cd3e72c7a1d3270f`
- 完整性：`git bundle verify` = okay
- 复验命令：`git bundle verify multigent.bundle && shasum -a 256 multigent.bundle`
- 恢复命令：`git clone multigent.bundle restored-repo`
- 注意：bundle 含 --all（所有本地分支/备份 ref）。作为许可证存证，核心 ref 是 `license-audit-baseline` tag 与其祖先链。
