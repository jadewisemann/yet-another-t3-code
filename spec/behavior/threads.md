# 스레드 명령 (`orchestration.dispatchCommand`, `orchestration.subscribeThread`)

P1에서는 YATT가 보내는 명령 5종(`thread.visit`, `thread.metadata.update`, `message.dispatch`, `thread.stop`, `thread.archive`)과 스레드 생성(`thread.create`, `orchestration.launchThread`)을 다룹니다. 63종 전체 목록은 `generated/rpc.json`의 `orchestration.dispatchCommand` payload에 있습니다.

- THREAD-1. `dispatchCommand`는 명령이 이벤트로 기록되면 `{"sequence":N}`으로 성공합니다. N은 그 명령이 만든 마지막 이벤트의 시퀀스입니다. (`thread-lifecycle`)
- THREAD-2. `thread.create`는 `projectId`가 가리키는 프로젝트가 없어도 성공합니다. 프로젝트 존재 여부를 확인하지 않습니다. (`dispatch-errors`)
- THREAD-2a. 없는 스레드에 대한 명령은 `OrchestrationV2DispatchCommandError`로 실패합니다. `message`와 `detail`은 `No orchestration projection exists for thread <id>.`이고, `cause`에 `OrchestratorProjectionError`와 그 아래 `ProjectionStoreThreadNotFoundError`가 중첩됩니다. (`dispatch-errors`)
- THREAD-3. `thread.visit`에는 `visitedAt`(ISO 8601 시각)이 반드시 있어야 합니다. (`thread-lifecycle`; 스펙 검증이 빠진 `visitedAt`을 실제로 잡아냈습니다.)
- THREAD-4. `subscribeThread`는 먼저 `{"kind":"snapshot","projection":{...}}`를 보냅니다. 새 스레드의 projection에는 `messages`, `runs`, `nodes` 등 빈 배열과 `thread` 객체가 있습니다. (`thread-lifecycle`)
- THREAD-5. `message.dispatch`, `thread.stop`, 제목 생성은 프로바이더가 필요하므로 P2에서 가짜 프로바이더와 함께 시나리오를 만듭니다. (시나리오 없음)
- THREAD-6. `launchThread`는 `threadId`를 주지 않으면 서버가 `thread:project:<projectId>:<uuid>` 형식의 id를 정합니다. `workspaceStrategy: root`이면 작업 폴더의 현재 git 브랜치를 스레드의 `branch`로 채웁니다. 성공 값은 `{threadId, resumed: false, projection}`이며 projection은 `subscribeThread` 스냅샷과 같은 모양입니다. 첫 메시지가 있는 실행은 P2에서 다룹니다. (`launch-thread`)
- THREAD-7. `launchThread` 직후 셸 스트림이 보내는 `thread.updated`의 `sequence`는 업스트림에서도 2 또는 3으로 달라집니다. 서버가 연속된 이벤트를 하나로 합치는 시점에 따라 달라지므로, 클라이언트는 이 값의 정확한 수에 의존하면 안 됩니다. (`launch-thread`)
