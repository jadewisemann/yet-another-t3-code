# 인증

- AUTH-1. `<bin> auth session issue --base-dir <dir> --token-only --label <l> --ttl <n>d`는 마지막 줄에 토큰 하나를 출력합니다. YATT `local.rs`와 conformance 러너가 이 형식에 의존합니다. (모든 시나리오가 이 명령으로 토큰을 받습니다.)
- AUTH-2. 발급한 토큰은 서버를 다시 시작해도 유효합니다. (시나리오 없음)
- AUTH-3. DPoP와 권한 범위(scope) 검사는 아직 구현하지 않습니다. Go 서버는 토큰 해시를 `userdata/sessions.json`에, 페어링 자격 증명 해시를 `userdata/pairings.json`에 저장합니다. (시나리오 없음)
- AUTH-4. `<bin> auth pairing create --json`은 `id`, `credential`(12자), `label`, `scopes`, `expiresAt`을 출력합니다. 기본 범위는 16개이며 순서도 업스트림과 같습니다. 기본 수명은 5분입니다. (`pairing`)
- AUTH-5. `POST /oauth/token`에 form으로 `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token=<credential>`, `subject_token_type=urn:t3:params:oauth:token-type:environment-bootstrap`을 보내면 `{access_token, expires_in: 2592000, issued_token_type, scope, token_type: "Bearer"}`를 받습니다. 받은 토큰으로 `/ws`가 열립니다. (`pairing`)
- AUTH-6. 같은 자격 증명을 다시 교환하면 `401`과 `{"_tag":"EnvironmentAuthInvalidError","code":"auth_invalid","reason":"invalid_credential","traceId":...}`를 받습니다. 자격 증명을 bearer 토큰으로 직접 쓰면 `/ws`가 `401`로 거절합니다. (`pairing`)
- AUTH-7. 서버는 실행되는 동안 `userdata/server-runtime.json`에 `version`(1), `host`, `port`, `origin`, `pid`, `ownerId`, `serverVersion`, `startedAt`, `managedPolicy`(null)를 기록합니다. YATT는 이 파일로 로컬 서버를 찾습니다. Go 서버는 종료할 때 이 파일을 지웁니다. (`runtime-state`)
