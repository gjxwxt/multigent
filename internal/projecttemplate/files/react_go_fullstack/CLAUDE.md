# Project CLAUDE.md

本文件为开发与调试速查手册。详细质量规范与架构约束见 `AGENTS.md`。

## 常用指令

```bash
# 本地开发双进程启动 (Go + Vite)
make dev

# 依赖安装
make install

# 单元测试与校验
make test
make verify

# 生产单二进制构建
make build

# 容器运行
docker compose -f deploy/compose.yml up --build
```
