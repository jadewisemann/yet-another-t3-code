# 스레드 명령 (`orchestration.dispatchCommand`, `orchestration.subscribeThread`)

P1에서는 YATT가 보내는 명령 5종(`thread.visit`, `thread.metadata.update`, `message.dispatch`, `thread.stop`, `thread.archive`)과 스레드 생성(`thread.create`, `orchestration.launchThread`)을 다룹니다. 63종 전체 목록은 `generated/rpc.json`의 `orchestration.dispatchCommand` payload에 있습니다.

- THREAD-1. `dispatchCommand`는 명령이 이벤트로 기록되면 `{"sequence":N}`으로 성공합니다. N은 그 명령이 만든 마지막 이벤트의 시퀀스입니다. (`thread-lifecycle`)
- THREAD-2. `thread.create`에는 `projectId`가 가리키는 프로젝트가 있어야 합니다. 프로젝트는 `projects.mutate`의 `project.create`로 만듭니다. (`thread-lifecycle`)
- THREAD-3. `thread.visit`에는 `visitedAt`(ISO 8601 시각)이 반드시 있어야 합니다. (`thread-lifecycle`; 스펙 검증이 빠진 `visitedAt`을 실제로 잡아냈습니다.)
- THREAD-4. `subscribeThread`는 먼저 `{"kind":"snapshot","projection":{...}}`를 보냅니다. 새 스레드의 projection에는 `messages`, `runs`, `nodes` 등 빈 배열과 `thread` 객체가 있습니다. (`thread-lifecycle`)
- THREAD-5. `message.dispatch`, `thread.stop`, 제목 생성은 프로바이더가 필요하므로 P2에서 가짜 프로바이더와 함께 시나리오를 만듭니다. (시나리오 없음)
