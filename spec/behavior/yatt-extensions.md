# YATT 확장

업스트림 T3 Code에 없거나, 업스트림과 다르게 동작하는 yatt-server의 기능입니다. 업스트림 클라이언트는 여기 있는 필드를 받아도 무시하고, 업스트림 서버는 여기 있는 명령과 메서드를 모르므로 YATT 앱은 그 실패를 조용히 넘깁니다. 적합성 시나리오가 아니라 Go 단위 테스트(`session_worktrees_test.go`)로 검증합니다.

## 정리(settle)와 섹션

- EXT-1. `thread.settle`과 `thread.unsettle` 명령은 업스트림과 같은 모양입니다. settle하면 셸 스레드의 `settledAt`이 그 시각(`settledAt`을 주면 그 값)이 되고 `settledOverride`는 `"settled"`가 됩니다. unsettle하면 `settledAt`은 `null`, `unsettledAt`은 그 시각, `settledOverride`는 `"active"`가 됩니다. 스레드의 `updatedAt`은 바뀌지 않습니다.
- EXT-2. yatt-server는 스레드를 스스로 settle하지 않습니다. 업스트림의 자동 settle(`thread.auto-settle`)은 구현하지 않으며, settle은 사용자의 명령으로만 일어납니다.
- EXT-3. `thread.section.set`(`threadId`, `section`)은 스레드를 사이드바 섹션에 넣습니다. 앞뒤 공백을 뺀 이름이 비었거나 `section`이 `null`이면 섹션에서 뺍니다. 섹션이 있는 스레드만 셸 스레드에 `section` 필드가 붙습니다. 섹션을 바꿔도 `updatedAt`은 바뀌지 않아서 목록 순서가 유지됩니다.

## 세션이 만든 worktree

- EXT-4. 턴이 시작되기 직전에 서버는 스레드 작업 폴더가 속한 저장소의 worktree 목록(`git worktree list`)과 HEAD를 기록합니다. 턴이 끝나면 목록을 다시 읽어서, 그사이에 새로 생긴 worktree를 그 스레드의 세션 worktree로 등록합니다. 메인 worktree, 스레드 자신의 작업 폴더, 프로젝트 폴더, 다른 스레드의 worktree는 제외합니다.
- EXT-5. 세션 worktree의 시작점(`base`)은 그 worktree의 HEAD와 턴 시작 때 스레드 작업 폴더 HEAD의 merge-base입니다. 이후 모든 턴의 시작과 끝에 세션 worktree를 체크포인트와 같은 방식(추적하지 않는 파일 포함, 무시된 파일 제외)으로 `refs/yatt/session-worktrees/<키>/turn/<n>/{start,end}`에 스냅숏합니다. 기록은 `userdata/session-worktrees/<키>.json`에 두며 이벤트 로그에는 남기지 않습니다.
- EXT-6. `orchestration.getTurnDiff`와 `getFullThreadDiff`의 성공 값에는, 요청한 턴 범위에서 바뀐 세션 worktree가 있을 때만 `worktrees: [{path, branch, diff}]`가 붙습니다. 범위가 0번 턴부터면 `base`부터, 아니면 범위 첫 턴의 시작 스냅숏부터 마지막 턴의 끝 스냅숏까지의 패치입니다. worktree 폴더가 지워졌어도 스냅숏 ref가 저장소에 남아 있으면 계속 보여 줍니다.

## 스레드의 PR

- EXT-7. `yatt.getThreadPullRequests`(`threadId`)는 스레드 작업 폴더와 세션 worktree들의 현재 브랜치마다 `gh pr list --head <브랜치> --state all`을 실행해서, 열린 PR을 먼저, 그다음 최근에 바뀐 순서로 돌려줍니다. 값은 `{threadId, pullRequests: [{number, title, url, state, isDraft, headRefName, updatedAt, worktreePath}], error?}`입니다. `gh`는 `YATT_GH_BIN`, `PATH`, `/opt/homebrew/bin`, `/usr/local/bin` 순서로 찾고, 없거나 실패하면 빈 목록과 `error`를 돌려줍니다. 브랜치가 없으면(detached HEAD) 빈 목록입니다.
