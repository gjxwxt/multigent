# 任务、预览与 Git 生命周期方案

本文定义 Multigent 中“初始化项目 → 创建任务 → Agent/智能助手修改 → 预览 → 完成 → 远程同步 → 新任务继承”的统一约定。

## 1. 先说结论

任务完成后应该同步远程仓库，但远程同步不是“新任务拿到最新代码”的唯一保证。真正的连续性保证是：

1. 创建任务时记录不可变的 `baseCommit`，并且从这个 SHA 创建任务分支；
2. 任务完成时生成 `completionCommit`，任务分支推送到远程并校验远端 SHA；
3. 新任务创建时重新 fetch，并明确从默认分支最新 SHA、或指定的上一个任务 `completionCommit` 开始；
4. 任务完成、远程同步、PR/MR 合并分别记录状态，不能因为一次网络失败就把本地完成状态改成失败，也不能把“本地完成”伪装成“远程已同步”。

快照默认采用 Git 的逻辑快照：`commit SHA + branch/ref + runtime metadata`。不为每个任务复制一份完整目录或 Docker 镜像。Git 对未变化的 blob 去重，活跃 worktree 和运行中容器才是主要的即时磁盘/内存消耗来源。

## 2. 分层模型

任务系统需要拆开以下生命周期：

```text
Project
 ├─ remote repository / default branch
 ├─ runtime contract
 └─ Task
     ├─ baseCommit
     ├─ task branch
     ├─ writable worktree
     ├─ preview session
     ├─ completionCommit
     ├─ remote sync
     └─ immutable snapshot
```

- **任务生命周期**：pending、in progress、awaiting confirmation、completed、failed。
- **Git 生命周期**：base SHA、task branch、completion SHA、remote branch、PR/MR。
- **Worktree 生命周期**：创建、使用、冻结、清理。
- **预览生命周期**：starting、web ready、API ready、ready、degraded、stopped、failed。
- **容器生命周期**：启动、租约续期、空闲停止、完成后停止、按快照重新打开。
- **远程同步生命周期**：pending、syncing、synced、failed、retrying。

它们之间有关联，但不能用一个状态字段互相代替。例如：任务可以已经本地完成，但远程同步暂时失败；任务可以已完成，但预览容器已停止。

## 3. 初始化项目

初始化阶段是后续确定性执行的基础：

1. 绑定 GitHub/GitLab provider、连接和远程仓库；凭据通过 credential helper 或 secretbox 注入，绝不写入 remote URL。
2. fetch 远程默认分支，记录默认分支名称和当前 SHA。
3. 写入 `.multigent/runtime.json`，声明开发/生产启动命令、服务目录、端口、健康检查路径和 SPA base 配置。
4. 运行确定性 doctor：文件存在性、命令可解析、依赖锁文件、构建命令、健康检查。
5. 生成骨架并完成初始化 commit，按项目策略推送默认分支。
6. 将远程 provider、repo、default branch、last observed SHA、runtime contract version 持久化到项目配置。

预览阶段不让 Agent 临时猜启动命令。解析优先级应为：

1. `.multigent/runtime.json`；
2. 明确的 package scripts / Makefile / 项目配置；
3. 有限的确定性文件探测；
4. 仅在无法判断时返回人工配置错误。

## 4. 创建任务与 Git 连续性

创建任务时：

1. 对远程默认分支执行 `git fetch origin <baseBranch>`；
2. 解析完整 `baseCommit`，不要只保存本地分支当前 SHA；
3. 从该 SHA 创建唯一任务分支和 worktree；
4. 保存 `baseBranch`、`baseCommit`、`branchName`、`worktreeDir`。

普通新任务默认从远程默认分支最新 SHA 开始。强依赖前一个任务时，显式从前一任务的 `completionCommit` 开始。禁止隐式使用 stale local `main`，也禁止仅凭“上一个任务已经完成”推断代码已进入默认分支。

## 5. 预览与智能助手

### 5.1 两种预览模式

- **开发预览**：绑定可写任务 worktree，启动 API + Web dev server，支持 HMR 和智能助手修改。
- **快照预览**：绑定某个 immutable commit，只读启动；任务完成后仍可以查看，但修改必须创建 follow-up task，不能直接污染完成快照。

开发预览是日常迭代主路径，生产预览用于验证 build 后的静态产物和单进程启动。

### 5.2 运行时契约

全栈项目至少声明：

```json
{
  "version": 1,
  "backend": { "directory": "server", "command": "go run .", "port": 8080, "healthPath": "/api/health" },
  "frontend": { "directory": "web", "command": "npm run dev -- --host 0.0.0.0 --port ${PORT}" },
  "preview": { "healthPath": "/", "startupTimeoutSeconds": 45 }
}
```

当前实现优先消费这个契约；如果文件存在但格式、版本或目录非法，会直接报错，不再悄悄退回猜测命令。契约不存在时才使用有限的确定性文件探测。`directory` 只能是 worktree 内的相对目录，`command` 由初始化流程或项目维护者明确提供。

当前 test11 类型项目中，后端必须在 `server` 目录执行 `go run .`；不能从根目录执行 `go run ./server/...`，因为 `server` 自己是 Go module 时根 module 不包含它。

### 5.3 就绪与路由

“docker run 返回”不等于“预览可用”。至少分阶段检查：进程存活、Web TCP、Web HTTP、API health、需要时的 HMR websocket。API 失败应该显示 `degraded` 或 `failed`，不能显示绿色 `running`。

外层路由必须和项目自身 API 分开：

- `/preview/<task>/*`：项目页面和项目资源；
- `/api/v1/projects/<project>/tasks/<task>/preview/*`：Multigent 预览控制、状态和智能助手；
- `/_multigent_preview/*`：注入的反馈/助手资源；
- 项目自己的 `/api/*`：继续交给项目 API/Vite proxy。

注入脚本只改写项目 URL，不得把 Multigent 的预览控制 API 改写成 `/preview/<task>/api/...`。SPA 则通过 runtime contract 提供 preview basename；控制台自身使用 `window.__MG_PREVIEW_BASE__` 兼容嵌套路由。

## 6. 完成、快照和远程同步

任务完成收口顺序：

1. 运行测试、构建和 runtime doctor；
2. 检查 worktree 无未提交改动；
3. 保存任务分支最新完整 HEAD 为 `completionCommit`；
4. push 任务分支到远程，不直接自动 push/覆盖 `main`；
5. 用 `git ls-remote` 校验远端分支确实指向该 SHA；
6. 更新 `remoteSyncStatus=synced` 和远端 SHA；
7. 冻结快照，停止开发容器；
8. 按工作流创建或更新 PR/MR，合并仍保留人工审核门禁。

如果 push 失败：本地任务仍可保持 completed，但 `remoteSyncStatus=failed`，保留错误信息和可重试任务。只有远端 SHA 校验通过才能显示“已同步”。

任务完成后的预览默认从 `completionCommit` 按需重建，并标记只读。若之后需要继续修改，就从该 SHA 创建新的 follow-up task。

## 7. 成本与清理策略

Git commit 本身通常很小，未变化文件共享对象；成本主要来自新产生的 blob、活跃 worktree、`node_modules`/构建缓存、日志和运行中的 Docker 容器。建议：

- `.gitignore` 排除依赖、dist、缓存、临时日志和大体积构建产物；
- npm/Go 缓存使用共享 volume，不进入任务快照；
- 远端分支 SHA 校验成功后，清理本地 worktree；
- 预览使用租约和空闲 TTL，页面/HMR/助手请求续租，完成后停止开发容器；
- Docker 容器加 project/task/session/snapshot labels，服务重启时按 label 回收孤儿容器；
- 未合并任务分支按可配置保留期清理，发布/钉住的快照永久保留；
- 定期执行 `git worktree prune` 和可控的 Git GC。

建议的初始默认值是预览空闲 20–30 分钟停止、未合并分支保留 30–90 天；这些都应做成配置，不写死在业务判断中。

## 8. 分阶段落地

### 第一阶段：连续性和当前预览故障

- 任务创建从远程最新 SHA 创建 worktree；
- 增加 completion/remote sync 字段；
- 修复全栈 Go 启动命令、错误输出和基础就绪检查；
- 修复预览助手控制 API 被项目代理吞掉的问题；
- 修复控制台 SPA preview basename。

### 第二阶段：runtime contract 和可靠预览

- 初始化骨架生成 `.multigent/runtime.json`（当前已支持在预览时读取和校验）；
- 补齐 dev/production 两种 plan；
- 增加结构化启动日志、API/Web/HMR readiness 和 degraded 状态；
- 增加当前项目形态的 E2E fixture。

### 第三阶段：任务完成同步与快照预览

- 完成 commit、push、远端 SHA 校验、重试和幂等；
- 完成任务后只读快照预览；
- 持久化 PreviewSession、租约、TTL 和孤儿容器回收。

### 第四阶段：PR/MR 闭环

- GitHub/GitLab provider 统一 push、创建/更新 PR/MR、读取状态、合并和分支清理（当前 GitLab 原有入口和 GitHub PR provider 已接入任务交付）；
- 任务完成、远端同步、PR/MR 合并三个状态分别展示；
- 默认分支合并后，下一任务自动从最新远端 SHA 创建。

## 9. 验收矩阵

最小验收必须覆盖：

| 场景 | 关键断言 |
| --- | --- |
| 新任务创建 | `baseCommit` 等于 fetch 后的远端 SHA |
| 全栈预览 | server 在正确 module 目录启动，Web 和 API health 都可达 |
| 智能助手 | 控制 API 不被改写到项目 `/preview/.../api` |
| SPA | preview basename 下首屏不是 404，内部导航仍留在 preview |
| 任务完成 | 保存 `completionCommit`，远端分支 SHA 可校验 |
| push 失败 | 本地完成状态保留，远程同步显示 failed 且可重试 |
| 新任务继承 | 从指定 completion SHA 或远端默认分支最新 SHA 创建 |
| 完成后预览 | 可从 commit 重建只读预览，不能直接修改完成快照 |
| 清理 | 空闲/完成容器停止，worktree 清理不影响 Git commit |
