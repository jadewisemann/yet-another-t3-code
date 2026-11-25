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
