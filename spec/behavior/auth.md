# 인증

- AUTH-1. `<bin> auth session issue --base-dir <dir> --token-only --label <l> --ttl <n>d`는 마지막 줄에 토큰 하나를 출력합니다. YATT `local.rs`와 conformance 러너가 이 형식에 의존합니다. (모든 시나리오가 이 명령으로 토큰을 받습니다.)
- AUTH-2. 발급한 토큰은 서버를 다시 시작해도 유효합니다. (시나리오 없음)
- AUTH-3. 업스트림의 페어링 링크, OAuth 토큰 교환, DPoP, 권한 범위(scope)는 P1 후반에 다룹니다. P0의 Go 서버는 `userdata/sessions.json`에 토큰 해시를 저장하는 단순한 방식만 구현합니다. (시나리오 없음)
