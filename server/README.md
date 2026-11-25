# yatt-server

T3 Code 서버의 Go 구현입니다. 지금은 P1 진행 중이며, 계획은 `docs/go-port/PLAN.md`, 구현 범위는 `spec/coverage.md`에 있습니다.

```sh
cd server
go test ./...
go run ./cmd/yatt-server serve --port 3773          # 기본 데이터 폴더: ~/.yatt/server
go run ./cmd/yatt-server auth session issue --token-only
```

| 경로 | 내용 |
|---|---|
| `cmd/yatt-server` | 서버. CLI는 업스트림 `t3`의 `serve`, `auth session issue`와 같은 형태입니다. |
| `cmd/t3conform` | 적합성 러너 (`conformance/README.md`) |
| `cmd/specdiff` | 두 `rpc.json`의 메서드 차이 |
| `cmd/speccov` | `spec/coverage.md` 생성 |
| `internal/wire` | Effect RPC JSON 프레이밍과 클라이언트 |
| `internal/spec` | `rpc.json` 로딩과 JSON Schema 검증 |
| `internal/conform` | 시나리오 실행, 정규화, 골든 비교 |
| `internal/server` | HTTP/WebSocket 서버와 RPC 핸들러 |
| `internal/store` | SQLite 이벤트 로그(`userdata/yatt-state.sqlite`)와 메모리 프로젝션 |
