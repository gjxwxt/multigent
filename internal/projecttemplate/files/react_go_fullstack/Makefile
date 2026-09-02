.PHONY: dev install test verify build doctor

dev:
	@trap 'kill 0' EXIT; (cd server && PORT=8080 go run .) & (cd web && npm run dev -- --host 0.0.0.0 --port 27891) & wait

install:
	@if [ -f web/package-lock.json ]; then cd web && npm ci; else cd web && npm install; fi
	cd server && go mod download

test:
	cd server && go test ./...
	cd web && npm run build

verify: doctor
	cd server && go test ./...
	cd web && npm run build

build:
	cd web && npm run build
	rm -rf server/webui/dist && mkdir -p server/webui/dist && cp -r web/dist/. server/webui/dist/
	cd server && go build -o bin/app .

doctor:
	@command -v go >/dev/null || (echo "doctor: go is required" && exit 1)
	@command -v npm >/dev/null || (echo "doctor: npm is required" && exit 1)
	@test -f .multigent/runtime.json || (echo "doctor: .multigent/runtime.json is missing" && exit 1)
	@test -f server/go.mod || (echo "doctor: server/go.mod is missing" && exit 1)
	@test -f web/package.json || (echo "doctor: web/package.json is missing" && exit 1)
	@test -f .gitlab-ci.yml || (echo "doctor: .gitlab-ci.yml is missing" && exit 1)
	@test -f deploy/Dockerfile || (echo "doctor: deploy/Dockerfile is missing" && exit 1)
	@test -f deploy/compose.yml || (echo "doctor: deploy/compose.yml is missing" && exit 1)
	@echo "doctor: ok"
