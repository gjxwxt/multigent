# 单镜像交付：二进制已内嵌前端产物（go:embed webui/dist），运行期无外部依赖。
# CI 路径：build:backend 产出 server/bin/app 并经 artifacts 传给本步（见 .gitlab-ci.yml）。
# 本地路径：先 `make build` 产出 server/bin/app，再 docker build -f deploy/Dockerfile .
FROM alpine:3.20
RUN adduser -D -g '' app
COPY server/bin/app /usr/local/bin/app
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/app"]
