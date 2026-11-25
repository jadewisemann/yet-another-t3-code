# 실행과 프로바이더 턴 (P2)

`message.dispatch`가 프로바이더 턴을 실행할 때의 동작입니다. 시나리오는 가짜 codex(`conformance/fixtures/fake-codex.mjs`)로 기록합니다. Codex와 주고받는 JSON-RPC는 `generated/codex.json`에 있습니다.

## Codex app-server와의 대화

- RUN-1. 서버는 `codex app-server`를 띄우고 `initialize`(`clientInfo.name: "T3 Code"`, `capabilities.experimentalApi: true`), `initialized` 알림 순서로 시작합니다. 프로바이더 상태 확인용 프로세스는 이어서 `account/read`, `skills/list`, `model/list`, `config/read`를 보냅니다. (`message-dispatch`의 fake-codex.log)
- RUN-2. 턴을 실행하는 프로세스는 `initialize`, `initialized`, `config/read`, `thread/start`, `thread/name/set`(스레드 제목), `config/read`, `turn/start` 순서로 보냅니다. `thread/start`의 `config`에는 `shell_environment_policy.set.T3CODE_THREAD_ID`로 스레드 id가 들어갑니다.
- RUN-3. 서버는 `turn/started`, `item/started`, `item/agentMessage/delta`, `item/completed`, `turn/completed` 알림을 받아 턴을 끝냅니다.

## 스레드 스트림에 나타나는 순서

한 번의 턴은 대략 다음 이벤트를 만듭니다: `provider-thread.updated`, `run.created`, `run-attempt.created`, `node.updated`(root), `checkpoint-scope.created`, `message.updated`(user), `turn-item.updated`, `provider-session.attached`/`updated`, `run.updated`(running), `provider-turn.updated`, `node.updated`(assistant), `message.updated`(assistant), `run.updated`(completed), `checkpoint.captured` 두 번, `turn-item.updated`(checkpoint).

- RUN-4. 사용자 메시지의 `message.updated`는 `id`가 `messageId`와 같고 `role: "user"`, `streaming: false`입니다. (`message-dispatch`)
- RUN-5. 어시스턴트 메시지의 id는 `message:provider:codex:native-item:<codex item id>`이고 `createdBy: "agent"`, `creationSource: "provider"`입니다. 완료되면 `streaming: false`와 전체 텍스트를 담습니다. (`message-dispatch`)
- RUN-6. 실행이 끝나면 `run.updated`의 `status`가 `completed`가 되고, 셸 스트림의 스레드 `status`도 `completed`가 됩니다. (`message-dispatch`)
- RUN-7. 턴이 끝난 뒤의 `subscribeThread` 스냅샷이 실행 결과 전체를 담습니다. 이 스냅샷이 Go 구현이 맞춰야 할 최종 상태입니다. (`message-dispatch`)

## id 형식

| 대상 | 형식 |
|---|---|
| run | `run:thread:<threadId>:ordinal:<n>` |
| run attempt | `run-attempt:run:<enc(runId)>:attempt:<n>` |
| root node | `node:run:<enc(runId)>:root` |
| provider item node | `node:provider:codex:native-item:<itemId>` |
| provider session | `provider-session:provider-instance:codex:shared` |
| provider thread | `provider-thread:provider:codex:native-thread:pending%3A<enc(runId)>` |
| provider turn | `provider-turn:provider:codex:native-turn:<turnId>` |
| turn item | `turn-item:message:<messageId>`, `turn-item:provider:codex:native-item:<itemId>` |
| checkpoint scope | `checkpoint-scope:thread:<threadId>:name:root` |
| checkpoint | `checkpoint:scope:<enc(scopeId)>:name:<n>` |

`enc`는 `encodeURIComponent`이고, 두 번 감싸면 두 번 인코딩됩니다(`%253A`). 체크포인트 git ref는 `refs/t3/orchestration-v2/checkpoints/<base64url(hex(sha256(scopeId))[:32])>/ordinal/<n>`입니다.

## 중지 (`thread.stop`)

- RUN-8. 실행 중인 턴에 `thread.stop`을 보내면 서버는 먼저 `turn-item:run:<enc(runId)>:signal:interrupt-request` 항목(`type: "run_interrupt_request"`, `title: "Interrupt requested"`, `message: "Stopped by user"`)을 기록하고, 그 이벤트의 시퀀스로 성공합니다. 그다음 codex에 `turn/interrupt`를 보냅니다. (`thread-stop`)
- RUN-9. codex가 `turn/completed`(`status: "interrupted"`)를 보내면 provider turn과 attempt가 `interrupted`가 되고, `…:signal:interrupt-result` 항목(`status: "interrupted"`, `title: "Interrupted"`, `parentItemId`는 요청 항목)이 생깁니다. 실행은 `waiting`을 거치지 않고 바로 `interrupted`로 끝나며, 그 뒤에도 체크포인트는 기록됩니다. 마지막 `run.updated`가 `checkpointId`를 채우고, 완료 경로와 달리 root node는 다시 갱신하지 않습니다. 셸 스트림에서는 `latestRunStopRequested`가 `true`가 됩니다. (`thread-stop`)

## yatt-server 구현 메모

- v2 이벤트(`run.*`, `node.updated`, `message.updated` 등)는 업스트림이 스레드 스트림에 보내는 봉투 그대로 저장하고, 업스트림의 `applyToProjection`처럼 id 기준 upsert로 접습니다(`server/internal/store/v2.go`). 스냅샷의 목록 순서는 업스트림 스냅샷 쿼리의 `ORDER BY`를 따릅니다.
- codex 프로세스는 프로바이더 인스턴스마다 하나를 공유합니다(`provider-session:provider-instance:codex:shared`). 실행 파일은 `PATH`의 `codex`이고, `YATT_CODEX_BIN`으로 바꿀 수 있습니다.
- 아직 다루지 않는 것: 실행 중에 보낸 메시지의 대기열(`queue_after_active`는 오류로 거절), 승인 요청과 사용자 입력 요청(codex의 서버 요청은 오류로 응답), 명령 실행과 파일 변경 같은 에이전트 메시지 이외의 항목, 체크포인트의 `files` 목록, T3 MCP 도구와 추가 지시문(`additionalContext`).
