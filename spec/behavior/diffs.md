# 턴 diff

`orchestration.getTurnDiff`와 `orchestration.getFullThreadDiff`는 스레드의 체크포인트 두 개 사이의 패치를 돌려줍니다. 업스트림 `CheckpointDiffQuery`와 같은 규칙을 따릅니다. (`turn-diff`)

- DIFF-1. 턴 번호는 체크포인트의 `appRunOrdinal`입니다. `status`가 `ready`이고, `appRunOrdinal`과 `runId`가 있으며, 그 실행이 `completed`인 체크포인트만 셉니다. `fromTurnCount`와 `toTurnCount`가 같으면 스레드를 찾지 않고 바로 빈 `diff`를 돌려줍니다. `getFullThreadDiff`는 `fromTurnCount`가 0인 `getTurnDiff`이며, `toTurnCount`가 0이면 빈 `diff`입니다.
- DIFF-2. 시작 체크포인트는 `fromTurnCount`가 0이면 root_run 범위의 0번 ref이고, 바로 앞 턴이면 같은 범위에서 순서가 하나 앞선 체크포인트이며, 그 밖에는 그 턴의 체크포인트입니다. 끝 체크포인트는 `toTurnCount` 턴의 체크포인트이고, 작업 폴더는 그 체크포인트 범위의 `cwd`입니다.
- DIFF-3. 패치는 `git diff --patch --src-prefix=a/ --dst-prefix=b/`로 만들고, `ignoreWhitespace`(기본값 참)이면 `--ignore-all-space`를 붙입니다. 체크포인트 사이에 HEAD가 움직였다면 턴이 쓰지 않은 경로는 빼고, 남은 경로를 pathspec 묶음으로 나누어 diff합니다. 결과가 10,000,000바이트를 넘으면 잘라 내고 `\n\n[truncated]`를 붙입니다.
- DIFF-4. 오류는 `OrchestrationGetTurnDiffError`(전체 diff는 `OrchestrationGetFullThreadDiffError`)이고, `message`는 `Failed to load turn diff`(`Failed to load full thread diff`)입니다. `cause`에는 체크포인트 오류의 `name`과 `message`가 들어갑니다. 스레드가 없으면 `CheckpointThreadNotFoundError`, 요청한 턴이 마지막 턴보다 크면 `CheckpointTurnRangeUnavailableError`, 체크포인트를 찾지 못하면 `CheckpointRefUnavailableError`입니다.
