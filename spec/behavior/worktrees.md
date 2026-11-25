# Worktree 스레드

`orchestration.launchThread`의 `workspaceStrategy`가 `worktree`나 `existing_worktree`이면, 스레드는 프로젝트 폴더 대신 git worktree에서 실행됩니다. (`worktree-launch`)

- WT-1. `worktree`로 시작하면 스레드는 `branch`가 요청한 브랜치(없으면 `t3/<16진수 8자리>`)이고 `worktreePath`가 `null`인 상태로 만들어집니다. 첫 실행의 `workspacePreparation`은 `{type: "worktree", baseRef, branch}`이며, 실행은 작업 공간이 준비될 때까지 `preparing`에 머뭅니다.
- WT-2. 서버는 `baseRef`에서 새 브랜치를 만들고 `<base-dir>/worktrees/<저장소 폴더 이름>/<브랜치>-<해시>`에 worktree를 만듭니다. 브랜치 부분은 `/`를 `-`로 바꾸고 23바이트에서 자르며, 해시는 `sha256(<git common dir>\0<브랜치>)`의 16진수 앞 16자리입니다. 같은 폴더가 이미 있으면 `-2`, `-3`처럼 번호를 붙입니다. `baseRef`가 없으면 HEAD에서 시작합니다.
- WT-3. worktree가 생기면 `<launch commandId>:workspace` 명령으로 `thread.metadata-updated`가 기록되어 스레드에 `branch`와 `worktreePath`가 붙습니다. 그다음 실행이 풀려나고, 체크포인트 범위의 `cwd`와 에이전트의 작업 폴더는 그 worktree입니다.
- WT-4. `existing_worktree`는 스레드를 처음부터 그 `worktreePath`와 브랜치(없으면 그 worktree에서 체크아웃된 브랜치)로 만들고, 루트 전략처럼 바로 실행합니다.
- WT-5. launchThread가 첫 메시지 id를 정하면 `message:thread:<threadId>:ordinal:1:<uuid>`이고, 사용자 메시지 항목의 id는 그 값을 인코딩한 `turn-item:message:<enc(messageId)>`입니다.
- 아직 다루지 않는 것: 원격에서 기준 브랜치를 가져오는 `startFromOrigin`, 서브모듈과 sparse checkout, 설정 스크립트, 임시 브랜치 이름을 첫 메시지로 다시 짓는 처리, `subscribeWorktreeSetup` 진행 상황 스트림, worktree 삭제.
