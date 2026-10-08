# CLAUDE.md - Development & Assistant Guidelines

本文件只是命令速查。所有架构规范、TDD 要求、文档所有权与交付检查清单**以 AGENTS.md 为准**——两份文件冲突时，AGENTS.md 是唯一事实源（本文件不再重复规范，防止双源漂移）。

## Quick Build & Test Commands
- **Backend Test**: `cd server && ./gradlew test`
- **Backend Single Test**: `cd server && ./gradlew test --tests "com.example.app.controller.ItemControllerTest"`
- **Backend Lint/Check**: `cd server && ./gradlew check -x test`
- **Backend Run**: `cd server && ./gradlew bootRun`
- **Frontend Test**: `cd web && npm test -- --run`
- **Frontend Lint**: `cd web && npm run lint`
- **Frontend Build**: `cd web && npm run build`
- **Docs Drift Gate**: `make docs-check`
- **Full Project Build**: `make build`
- **Full Project Test**: `make test`

## Before You Commit
按 AGENTS.md §7 交付检查清单逐条核实（测试全绿、目录归属、无凭据、api-spec 同步 + docs-check 通过）。
