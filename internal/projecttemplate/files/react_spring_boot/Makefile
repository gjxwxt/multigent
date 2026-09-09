.PHONY: all dev dev-backend dev-frontend install test test-backend test-frontend lint verify build build-frontend build-backend clean doctor

all: build

dev:
	@echo "Starting development environment..."
	@echo "Backend: cd server && ./gradlew bootRun"
	@echo "Frontend: cd web && npm run dev"

dev-backend:
	cd server && ./gradlew bootRun

dev-frontend:
	cd web && npm run dev

install:
	@test -f web/package-lock.json || (echo "error: web/package-lock.json is required for deterministic install" && exit 1)
	cd web && npm ci
	cd server && ./gradlew --no-daemon dependencies

test: test-backend test-frontend

test-backend:
	cd server && ./gradlew --no-daemon test

test-frontend:
	cd web && npm test -- --run

lint:
	cd server && ./gradlew --no-daemon check -x test
	cd web && npm run lint

verify: doctor lint test build-frontend
	@echo "verify: ok"

build: build-frontend build-backend

build-frontend:
	@if [ ! -d web/node_modules ]; then $(MAKE) install; fi
	cd web && npm run build

build-backend:
	mkdir -p server/src/main/resources/static
	cp -r web/dist/* server/src/main/resources/static/
	cd server && ./gradlew --no-daemon bootJar -x test
	mkdir -p build/libs && cp server/build/libs/*.jar build/libs/app.jar

doctor:
	@command -v java >/dev/null || (echo "doctor: java is required" && exit 1)
	@command -v npm >/dev/null || (echo "doctor: npm is required" && exit 1)
	@test -f .multigent/runtime.json || (echo "doctor: .multigent/runtime.json is missing" && exit 1)
	@test -f server/build.gradle || (echo "doctor: server/build.gradle is missing" && exit 1)
	@test -f server/gradle/wrapper/gradle-wrapper.jar || (echo "doctor: server/gradle/wrapper/gradle-wrapper.jar is missing" && exit 1)
	@test -x server/gradlew || (echo "doctor: server/gradlew is not executable" && exit 1)
	@test -f web/package.json || (echo "doctor: web/package.json is missing" && exit 1)
	@test -f web/package-lock.json || (echo "doctor: web/package-lock.json is missing" && exit 1)
	@test -f .gitlab-ci.yml || (echo "doctor: .gitlab-ci.yml is missing" && exit 1)
	@test -f deploy/Dockerfile || (echo "doctor: deploy/Dockerfile is missing" && exit 1)
	@test -f deploy/compose.yml || (echo "doctor: deploy/compose.yml is missing" && exit 1)
	@echo "doctor: ok"

clean:
	cd server && ./gradlew --no-daemon clean
	rm -rf web/dist web/node_modules server/src/main/resources/static/* build/libs
