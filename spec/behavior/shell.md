# 셸 스트림 (`orchestration.subscribeShell`)

셸은 프로젝트 목록과 스레드 요약 목록입니다. YATT의 사이드바가 이 스트림만으로 그려집니다.

- SHELL-1. 구독하면 먼저 `{"kind":"snapshot","snapshot":{...}}`를 보냅니다. 스냅샷에는 `schemaVersion`(현재 2), `snapshotSequence`, `projects`, `threads`, `archivedThreads`가 있습니다. (`shell-empty`)
- SHELL-2. 빈 데이터 폴더의 `snapshotSequence`는 0입니다. 이후 명령이 하나 처리될 때마다 이벤트 시퀀스가 1씩 늘어나고, 스냅샷의 `snapshotSequence`는 그 시점의 시퀀스입니다. (`shell-empty`, `thread-lifecycle`)
- SHELL-3. payload에 `requestCompletionMarker: true`가 있으면 첫 스냅샷 다음에 `{"kind":"synchronized"}`를 보냅니다. YATT처럼 `{}`를 보내면 `synchronized`는 오지 않습니다. (`shell-empty`)
- SHELL-4. 프로젝트가 생기거나 바뀌면 `{"kind":"project.updated", ...}`를 보냅니다. (`project-create`)
- SHELL-5. 스레드가 생기거나 제목 등이 바뀌면 `{"kind":"thread.updated","location":"active","sequence":N,"thread":{...}}`를 보냅니다. (`thread-lifecycle`)
- SHELL-6. 스레드를 보관하면 `{"kind":"thread.removed","location":"active","threadId":...}`를 보냅니다. 보관 목록에 추가되었다는 별도 값은 이 스트림에 오지 않습니다. (`thread-lifecycle`)
- SHELL-7. `thread.visit`는 셸 스트림에 아무 값도 보내지 않습니다. (`thread-lifecycle`)
- SHELL-8. 첫 스레드가 생긴 직후 서버는 `resolvedRepositoryIdentityRoots`가 붙은 스냅샷을 한 번 더 보냅니다. 클라이언트는 이미 받은 스냅샷을 이것으로 교체해야 합니다. 이 스냅샷의 `snapshotSequence`와 포함되는 스레드는 업스트림에서도 타이밍에 따라 달라집니다. 스냅샷이 오는 위치도 첫 `thread.updated`의 앞이나 뒤로 달라지므로, 시나리오는 이 스냅샷을 따로 떼어 확인합니다(`aside`). (`thread-lifecycle`, `launch-thread`)
