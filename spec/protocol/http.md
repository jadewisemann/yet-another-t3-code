# HTTP 경로

RPC 그룹 밖에 있는 경로입니다. P1에서 필요한 경로만 먼저 정리했고, 나머지는 해당 단계를 시작할 때 업스트림 `apps/server/src`의 `HttpRouter.add` 호출에서 목록을 뽑아 채웁니다.

| 경로 | 단계 | 내용 |
|---|---|---|
| `GET /.well-known/t3/environment` | P0 | 인증 없이 답합니다. 최상위 키는 `environmentId`, `label`, `platform`, `serverVersion`, `orchestrationProtocolVersion`, `databaseMigration`, `capabilities`입니다. YATT는 이 응답으로 로컬 서버가 떠 있는지 확인합니다. (`environment-descriptor`) |
| `GET /ws` | P0 | `protocol/wire.md` 참고 |
| `POST /oauth/token` | P1 | `t3 pair` 링크의 토큰을 세션 토큰으로 교환합니다. YATT의 원격 연결이 사용합니다. |
