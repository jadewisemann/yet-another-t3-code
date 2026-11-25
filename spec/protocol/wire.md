# WebSocket RPC 전송 규칙

업스트림 0.1.11에서 확인한 내용이며, `conformance/` 시나리오가 각 규칙을 검증합니다. 구현은 `server/internal/wire`와 `server/internal/server/server.go`에 있습니다.

## 연결

- W1. 엔드포인트는 `GET /ws`이고, 쿼리에 `orchestrationProtocol=2`가 있어야 합니다. 이 값이 없거나 다르면 업그레이드하지 않고 `426`과 다음 JSON을 보냅니다. YATT는 이 본문을 사용자에게 보여 줍니다. (`ws-protocol-mismatch`)

  ```json
  {"code":"orchestration_protocol_incompatible","message":"Update this client to one that supports orchestration protocol 2.","orchestrationProtocolVersion":2}
  ```

- W2. 프로토콜 버전이 맞더라도 `Authorization: Bearer <token>`이 유효하지 않으면 `401`로 거절합니다. (`ws-requires-auth`)
- W3. 클라이언트는 `clientSurface`, `clientAppVersion`, `clientCapabilities` 쿼리도 보낼 수 있습니다. 서버는 알 수 없는 값이 와도 연결을 거절하지 않습니다.

## 프레임

텍스트 프레임 하나에 JSON 객체 하나가 들어가고, `_tag`로 종류를 구분합니다.

| 방향 | `_tag` | 필드 |
|---|---|---|
| 클라이언트 → 서버 | `Request` | `id`(문자열 또는 숫자), `tag`(메서드 이름), `payload`, `headers`(배열, 비어 있어도 반드시 포함) |
| 클라이언트 → 서버 | `Ack` | `requestId` |
| 클라이언트 → 서버 | `Interrupt` | `requestId`, `interruptors`(배열) |
| 클라이언트 → 서버 | `Ping`, `Eof` | 없음 |
| 서버 → 클라이언트 | `Chunk` | `requestId`, `values`(비어 있지 않은 배열) |
| 서버 → 클라이언트 | `Exit` | `requestId`, `exit` |
| 서버 → 클라이언트 | `Defect` | `defect` (연결 전체의 오류) |
| 서버 → 클라이언트 | `Pong` | 없음 |

- F1. `headers`가 빠진 `Request`는 해당 요청이 `Die("Invalid request headers")`로 끝납니다. 이 규칙은 conformance 러너를 작성하면서 실제로 확인했습니다.
- F2. `exit`는 `{"_tag":"Success","value":...}` 또는 `{"_tag":"Failure","cause":[...]}`입니다. `cause` 항목은 `Fail`(`error`에 `_tag`가 있는 타입 오류), `Die`(`defect`에 문자열), `Interrupt`(`fiberId`) 중 하나입니다.
- F3. 서버가 모르는 메서드는 `Failure` + `Die("Unknown request tag: <method>")`로 끝납니다. (`unknown-method`)
- F4. 스트림 메서드는 값이 생길 때마다 `Chunk`를 보내고, 클라이언트가 `Ack`를 보내기 전에는 다음 `Chunk`를 보내지 않습니다.
- F5. 구독 스트림은 스스로 끝나지 않습니다. 클라이언트가 `Interrupt`를 보내면 `Failure` + `Interrupt` cause로 끝납니다. (`shell-empty`)
- F6. `Ping`에는 `Pong`으로 답합니다.

## 성공 값과 오류의 모양

메서드별 payload, 성공 값(스트림이면 chunk 값 하나), `Fail` 오류의 모양은 `spec/generated/rpc.json`이 정합니다. conformance 러너는 서버가 보내는 모든 값을 이 스키마로 검증하고, 맞지 않으면 시나리오를 실패로 처리합니다.
