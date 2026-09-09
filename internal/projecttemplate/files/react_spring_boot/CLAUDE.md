# CLAUDE.md - Development & Assistant Guidelines

## Quick Build & Test Commands
- **Backend Test**: `cd server && ./gradlew test`
- **Backend Single Test**: `cd server && ./gradlew test --tests "com.example.app.controller.ItemControllerTest"`
- **Backend Lint/Check**: `cd server && ./gradlew check -x test`
- **Backend Run**: `cd server && ./gradlew bootRun`
- **Frontend Test**: `cd web && npm test -- --run`
- **Frontend Lint**: `cd web && npm run lint`
- **Frontend Build**: `cd web && npm run build`
- **Full Project Build**: `make build`
- **Full Project Test**: `make test`

## Architecture & Code Rules
- **Backend (Spring Boot 3.3.3 / Java 21 / Gradle)**:
  - Strict layered architecture: `controller/` -> `service/` -> `repository/` -> `model/`.
  - Use Java 21 `record` for DTOs and API requests/responses.
  - Return uniform `ApiErrorResponse` on failures through `@RestControllerAdvice` in `exception/`.
  - Controllers must only validate inputs, call services, and return DTOs with proper HTTP status codes.
- **Frontend (React 18 / TypeScript / Vite / Tailwind)**:
  - Separate `components/`, `pages/`, `services/`, `types/`, and `test/`.
  - All network requests go through `services/api.ts` with TypeScript types.
- **TDD Requirement**:
  - Always write failing tests first verifying acceptance criteria before implementing business code.
  - Keep test assertions meaningful and realistic.
