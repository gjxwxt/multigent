# 生产镜像交付：运行期基于轻量 eclipse-temurin:21-jre-alpine。
# CI 路径：build:backend 产出 build/libs/app.jar 并经 artifacts 传给本步（见 .gitlab-ci.yml）。
# 本地路径：先 `make build` 产出 build/libs/app.jar，再 docker build -f deploy/Dockerfile .
FROM eclipse-temurin:21-jre-alpine
RUN adduser -D -g '' app
WORKDIR /app
COPY build/libs/app.jar app.jar
RUN chown -R app:app /app
USER app
EXPOSE 8080
ENV JAVA_OPTS="-XX:+UseContainerSupport -XX:MaxRAMPercentage=75.0"
ENTRYPOINT ["sh", "-c", "java $JAVA_OPTS -jar /app/app.jar"]
