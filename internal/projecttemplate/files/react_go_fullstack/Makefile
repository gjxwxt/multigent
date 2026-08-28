.PHONY: dev test build doctor

dev:
	@trap 'kill 0' EXIT; (cd server && PORT=8080 go run .) & (cd web && npm run dev -- --host 0.0.0.0 --port 27891) & wait

test:
	cd server && go test ./...
	cd web && npm run build

build:
	cd web && npm run build

doctor:
	@command -v go >/dev/null || (echo "doctor: go is required" && exit 1)
	@command -v npm >/dev/null || (echo "doctor: npm is required" && exit 1)
	@test -f .multigent/runtime.json || (echo "doctor: .multigent/runtime.json is missing" && exit 1)
	@test -f server/go.mod || (echo "doctor: server/go.mod is missing" && exit 1)
	@test -f web/package.json || (echo "doctor: web/package.json is missing" && exit 1)
	@echo "doctor: ok"
