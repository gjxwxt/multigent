# Multigent 平台离线降级与独立工具箱操作手册 (Platform Offline Fallback Runbook)

本文档面向研发工程师、系统运维与紧急抢修人员，提供在 **Multigent 控制台服务宕机、不可达或离线维护** 场景下的标准作业程序 (SOP)。

通过本手册，操作者可在**完全脱离 Multigent 控制台 Web 服务与服务端进程**的前提下，使用本地命令行工具完成**代码确定性检查**、**安全合规打标与生产发布**以及**独立容器化预览复现**。

---

## 0. 适用场景与安全红线 (Scope & Invariants)

### 适用场景
1. **控制台停机/故障窗口**：Multigent 后端服务异常崩溃或正在重启维护，但有紧急线上 Hotfix 需要立即验证并发布。
2. **离线单人排查**：开发者在个人工作站上需要快速复现沙箱容器行为，无需占用平台任务配额与临时端口租期。
3. **Phase 1 CLI 验收规格**：本手册中明确的手工命令序列，作为后续 `mgt` 独立命令行工具箱（Phase 1+）的确定性验收基准。

### 安全红线 (Security Invariants — 违反即事故)
1. **凭据永不导出 (No Exported Secrets)**：
   - 离线降级完全基于**操作者本人的 GitLab 权限与工作站凭据**（SSH Key、Personal Access Token 或 `glab` CLI 配置）。
   - **严禁**从控制台数据库（`multigent.db`）中导出或解密平台托管的机器 Token。平台不可用时，权限回归至 GitLab 项目自身的人员角色（Maintainer+ 打标权限），这是天然的 Fail-Safe 机制。
2. **严禁硬编码环境机密 (No Secrets in Working Tree)**：
   - 所有脚本与命令中严禁包含内网真实宿主 IP、生产 Token 或内部凭据，一律使用规范占位符（如 `<repo-dir>`, `<commit-sha>`, `<gitlab-host>`）。
3. **确定性发布基线 (Deterministic Release Baseline)**：
   - 发布打标必须基于**已合入主干（main）的不可变 Commit SHA**。
   - 严禁在未合入 main 的临时特性分支、Worktree 或浮动的动态分支引用上直接打 Release Tag。
4. **诚实边界声明 (Honest Boundaries)**：
   - 离线工具箱保障的是“单人能够把经过严格验证的代码发布出去”，而非“离线模拟平台的所有协作能力”。
   - 人机协作状态机、跨角色审批闸门、RBAC 细粒度校验与集中式审计事件在离线时不可用；恢复后需手工在对应任务中对账留痕。

---

## 1. 本地确定性 CI/CD 检查 SOP (Local CI Readiness Gate)

在 Multigent 体系中，代码在发运前必须通过确定性 CI/CD 准入门禁（CI Ready Gate）。该门禁在代码内核（`internal/ciready`）中是**完全无状态的纯函数**，只依赖目标仓库的文件树。

### 1.1 核心检查规则清单 (10 项纯函数规则)

在无控制台时，必须确保目标仓库满足以下 10 项静态契约：

| 检查项 | 判定规则 | 失败影响与排查指南 |
|---|---|---|
| `baseline_files` | 根目录必须存在 `.gitlab-ci.yml`，且包含 `deploy/Dockerfile` 与 `deploy/compose.yml`。 | 缺失时会导致 CI 流程无模板或容器化镜像无法打包。 |
| `ci_yaml_parses` | `.gitlab-ci.yml` 必须符合有效 YAML 格式规范。 | 语法错误将导致 GitLab CI 直接在 Lint 阶段报解析失败。 |
| `required_jobs` | 必须包含以下 6 个核心 Job：<br/>1. `lint:backend`<br/>2. `test:backend`<br/>3. `build:frontend`<br/>4. `build:backend`<br/>5. `package`<br/>6. `deploy` | 缺失任一核心 Job 即破坏平台交付基线契约。 |
| `runner_tags` | 所有声明的 Job 必须显式携带 `tags: [docker]`。 | 无此标签会导致任务无法被专用的 Docker Runner 认领。 |
| `tag_only_release` | `package` 和 `deploy` 两个阶段必须严格配置 `` 规则门禁。 | 防止向主分支（main）日常推送提交时误触发打包与部署。 |
| `npm_mirror` | 前端构建脚本中必须显式声明使用 `registry.npmmirror.com`。 | 避免海外官方源冷拉依赖耗时超 5–7 分钟导致构建超时。 |
| `apk_cache` | 涉及 Alpine 镜像安装命令（`apk add`）时，**严禁**使用 `--no-cache`，必须配置 `--cache-dir /cache/apk`。 | 避免在每个 Job 中重复从外网下载 Docker CLI。 |
| `interruptible` | 必须在全局或各个 Job 配置 `default.interruptible: true`。 | 允许后序提交自动取消前序冗余流水线，节约 Runner 资源。 |
| `frontend_scripts` | 前端 `package.json` 的 `scripts` 节点必须包含 `"lint"` 指令。 | 配合 `lint:backend` 保证全栈代码风格静态门禁。 |
| `health_path` | 后端服务必须配置健康的探活端点（如 `/actuator/health` 或 `/healthz`）。 | 保证部署后容器探针能够获得确定性的存活反馈。 |

### 1.2 本地单行校验脚本 (Zero-Server Execution)

若机器上具备 Go 运行环境，可直接调用仓库内置的 `ciready` 纯函数模块，执行零服务静态校验：

```bash
# 进入 multigent 仓库目录，执行对目标工程的就绪检查
go run -C /Users/imac/Documents/code/github/multigent -e '
package main
import (
	"fmt"
	"os"
	"github.com/multigent/multigent/internal/ciready"
)
func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run ... <target-repo-dir>")
		os.Exit(2)
	}
	report := ciready.Verify(os.Args[1])
	fmt.Printf("
=== CI/CD Readiness Report: %s (Overall: %s) ===
", report.Repo, report.Overall)
	hasFail := false
	for _, c := range report.Checks {
		statusMark := "✓"
		if c.Status == ciready.StatusFail {
			statusMark = "✗"
			hasFail = true
		} else if c.Status == ciready.StatusSkip {
			statusMark = "-"
		}
		detail := ""
		if c.Detail != "" {
			detail = " -> " + c.Detail
		}
		fmt.Printf("[%s] %-20s : %s%s
", statusMark, c.Name, c.Status, detail)
	}
	if hasFail {
		os.Exit(1)
	}
}
' /path/to/target-project-repo
```

### 1.3 缺失基线文件补全 (Idempotent Seed)
若新工程缺少 `.gitlab-ci.yml` 或 `deploy/` 目录：
1. 从平台内置模板基线中复制（或执行 `ciready.Ensure(repoDir)`）；
2. **幂等原则**：已有文件绝不覆写，仅补齐缺失的骨架文件；
3. 补齐后执行 `git status` 与 `git diff` 确认差异并提交。

---

## 2. 离线合规打标与发布 SOP (Offline Tag & Release SOP)

在 Multigent 架构中，**GitLab CI 是实际的发布执行者**，平台本身并不直接向宿主执行 Docker 发布，而是通过推送附注 Git Tag 触发 GitLab 流水线中的 `package` 与 `deploy` 任务。

因此，在控制台宕机时，可通过操作者个人权限执行合规发布：

### 2.1 发布前置条件核对
- [ ] 本地代码已全部完成编译、单测与 Lint 校验（`make test` 或对应工程构建通过）。
- [ ] 本地已完成上述 §1 的 10 项 CI Ready 静态检查。
- [ ] 目标变更已合入 `main` 分支，且工作区干净。

### 2.2 确定性基线核验与打标步骤

```bash
# 1. 切换至待发布的仓库目录
cd /path/to/target-project-repo

# 2. 确保切到 main 并拉取最新主干提交
git checkout main
git pull origin main

# 3. 记录目标发布 Commit SHA
TARGET_SHA=$(git rev-parse HEAD)
echo "Target Release Commit SHA: $TARGET_SHA"

# 4. 硬性校验：确保该 Commit SHA 属于 main 分支祖先链（严禁在独立分支打标）
git merge-base --is-ancestor "$TARGET_SHA" main || {
    echo "FATAL: Target commit $TARGET_SHA is not an ancestor of main branch!"
    exit 1
}

# 5. 定义版本号（必须严格遵守 SemVer 规范，如 v1.0.0）
RELEASE_TAG="v1.0.0"

# 6. 创建附注 Tag（Annotated Tag，严禁创建轻量标签）
git tag -a "$RELEASE_TAG" "$TARGET_SHA" -m "Release $RELEASE_TAG: offline emergency deployment"

# 7. 推送 Tag 至 GitLab 远端（触发 tag 流水线）
git push origin "$RELEASE_TAG"
```

### 2.3 监控发布流水线状态

推送 Tag 后，GitLab CI 会自动启动 Tag Pipeline：

1. **使用 GitLab CLI (`glab`) 监控**：
   ```bash
   # 查看当前仓库最新的流水线状态
   glab ci status --live
   
   # 或查看指定 tag 的流水线详情
   glab pipeline list --ref "$RELEASE_TAG"
   ```
2. **使用 Web 界面监控**：
   - 打开 `https://<gitlab-host>/<group>/<project>/-/pipelines`；
   - 确认包含当前 `$RELEASE_TAG` 的流水线；
   - 确认 `lint:backend`、`test:backend`、`build:frontend`、`build:backend`、`package`、`deploy` 状态全部为绿色（`passed`）。
3. **失败处置**：
   - 依据 AGENTS.md §6 准则：若 `deploy` job 失败，如实摘录 GitLab Job 输出日志排查（如 Runner 宿主 Docker 磁盘满、端口冲突等），禁止隐瞒；
   - 如需回滚，切勿直接就地改代码打相同的 Tag，应修复后推进次版本号或按回滚预案切换前一稳定 Tag。

---

## 3. 独立容器预览复现 SOP (Standalone Container Preview SOP)

当需要本地验证页面渲染、排查前后端连通性或复现缺陷，而 Multigent 控制台服务未启动时，可直接通过本地 Docker 启动完全隔离的预览容器。

### 3.1 读取运行时契约 (`.multigent/runtime.json`)

Multigent 的所有预览行为均由项目根目录下的 `.multigent/runtime.json` 唯一定义。首先检查该文件：

```bash
cat .multigent/runtime.json
```
典型的契约内容示例：
```json
{
  "version": "1.0",
  "templateId": "react-go-fullstack",
  "port": 3000,
  "backend": {
    "directory": "server",
    "command": "go run .",
    "port": 8080,
    "healthPath": "/healthz"
  },
  "frontend": {
    "directory": "web",
    "command": "npm run dev",
    "port": 3000
  }
}
```

### 3.2 准备平台统一缓存卷 (One-Time Setup)

为了保证构建速度与依赖复用，创建与平台 preview engine 完全同名的 Docker 缓存卷：

```bash
docker volume create multigent-toolchains
docker volume create multigent-npm-cache
docker volume create multigent-go-cache
docker volume create multigent-go-build-cache
```

### 3.3 启动独立预览容器

执行以下标准命令，该命令与 `internal/preview/engine.go` 中的 `previewDockerBaseArgs` 保持完全一致的隔离与挂载参数：

```bash
PREVIEW_PORT=3000
CONTAINER_NAME="standalone-preview-$(basename $(pwd))"

docker run -d   --name "$CONTAINER_NAME"   -p "127.0.0.1:${PREVIEW_PORT}:${PREVIEW_PORT}"   -v "$(pwd):/workspace"   -v "multigent-toolchains:/opt/multigent/toolchains"   -v "multigent-npm-cache:/root/.cache/npm"   -v "multigent-go-cache:/root/go/pkg/mod"   -v "multigent-go-build-cache:/root/.cache/go-build"   -e "npm_config_cache=/root/.cache/npm"   -e "GOPATH=/root/go"   -e "GOMODCACHE=/root/go/pkg/mod"   -e "GOCACHE=/root/.cache/go-build"   -e "PORT=${PREVIEW_PORT}"   -e "GOFLAGS=-buildvcs=false"   -w "/workspace"   ghcr.io/multigent/multigent/runtime-base:latest   sh -c "(cd web && npm install --no-audit --no-fund && npm run dev -- --port 3000 --host 0.0.0.0) & (cd server && go run .) & wait"
```
*(注：对于 Spring Boot / JVM 项目，将镜像替换为 `ghcr.io/multigent/multigent/runtime-jvm21:2026.9.1` 并按 gradle bootRun 启动)*

### 3.4 探活与验证

1. **查看启动日志**：
   ```bash
   docker logs -f "$CONTAINER_NAME"
   ```
2. **本地探针访问**：
   ```bash
   # 校验前端界面
   curl -I "http://127.0.0.1:${PREVIEW_PORT}/"
   
   # 校验后端接口/健康检查
   curl -f "http://127.0.0.1:${PREVIEW_PORT}/healthz"
   ```
3. **浏览器打开**：
   在浏览器中直接访问 `http://127.0.0.1:3000` 即可交互验证。

### 3.5 销毁与资源释放

验证完成后，务必手动清理容器，释放宿主端口：

```bash
docker stop "$CONTAINER_NAME" && docker rm "$CONTAINER_NAME"
```

---

## 4. 平台恢复后状态对账与留痕 (Post-Recovery Reconciliation)

当 Multigent 控制台服务恢复上线后，操作者应完成以下对账工作，避免平台状态与物理现实脱节：

1. **代码基线与 Tag 同步**：
   - 离线推送的 Git Tag 与 main 分支提交已经持久化在 GitLab 远端。
   - 平台恢复后，下次任务或工作流初始化时会自动拉取远端更新，无需额外手工同步 Git 树。
2. **任务留痕与证据关联**：
   - 若本次离线发布关联了正在进行中的平台任务（如处于 `release` 或 `go_live_confirm` 节点）：
   - 在任务卡片或评论区中，显式填入离线发布的客观证据：
     - **发布 Tag**：如 `v1.0.0`
     - **GitLab Pipeline URL**：如 `https://<gitlab-host>/<group>/<project>/-/pipelines/1234`
     - **健康检查结果**：如 `HTTP 200 OK (/healthz)`
     - **操作声明**：注明“因平台维护窗口，本次发布经《平台离线降级 Runbook》手工执行并通过验证”。
3. **未完成工作流流转**：
   - 在控制台对应的人工审核节点（`human_review`）点击 Approve 推进，将状态机置为完成，形成完整的审计闭环。
