# 동작 스펙

도메인마다 파일 하나를 두고, 규칙에 `영역-번호` 형식의 번호를 붙입니다. 규칙마다 그 규칙을 검증하는 시나리오 이름을 괄호로 적고, 아직 시나리오가 없으면 `(시나리오 없음)`이라고 적습니다. 규칙은 업스트림 코드가 아니라 관찰한 동작(골든 기록)과 계약 스키마를 근거로 씁니다.

새 동작을 포팅하는 순서는 다음과 같습니다.

1. 업스트림 코드와 실제 동작을 확인해서 이 폴더에 규칙을 씁니다.
2. 규칙을 검증하는 시나리오를 `conformance/scenarios/`에 추가하고, `scripts/conformance.sh record`로 업스트림에서 골든 기록을 만듭니다.
3. 그 시나리오를 `conformance/expected-failures.txt`에 넣은 상태로 Go 구현을 시작합니다.
4. 시나리오가 통과하면 목록에서 지웁니다. CI는 목록에 있는 시나리오가 통과하면 실패하므로, 목록은 줄어들기만 합니다.

| 파일 | 단계 |
|---|---|
| `shell.md` | P1 |
| `threads.md` | P1 |
| `auth.md` | P1 |
| `runs.md` | P2 |
| `titles.md` | P2 |
| `diffs.md` | P3 |
| `worktrees.md` | P3 |
| `attachments.md` | P3 |
| `terminals.md` | P3 |
| `yatt-extensions.md` | YATT 전용 (업스트림에 없음) |
