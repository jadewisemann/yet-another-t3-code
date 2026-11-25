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

## 대기열 (`queue_after_active`)

- RUN-10. 실행이 활성 상태(`preparing`, `queued`, `starting`, `running`, `waiting`)일 때 보낸 메시지는 `dispatchMode`가 `queue_after_active`든 `start_immediately`든 대기열에 들어갑니다. 대기 실행은 기존 provider thread를 쓰고, `run.created`(`status: "queued"`, `queuePosition`은 대기 중인 실행들의 위치 최댓값 + 1), `run-attempt.created`, `node.updated`(`pending`), `checkpoint-scope.created`, `message.updated`만 기록합니다. provider thread 이벤트와 사용자 turn item은 아직 없습니다. (`message-queue`)
- RUN-11. 활성 실행이 체크포인트까지 끝나면, 가장 앞선 대기 실행이 시작됩니다. 오케스트레이터 이벤트로 provider thread가 `not_loaded`(`lastRunOrdinal`은 새 실행, `driver` 없음)가 되고, `inputIntent: "queued_turn"`인 사용자 turn item과 `run.updated`(`starting`, `queuePosition: null`)가 이어집니다. 그다음은 일반 실행과 같습니다. 이미 로드된 codex 스레드에는 `config/read` 한 번 뒤에 `turn/start`만 보냅니다. (`message-queue`)
- RUN-12. 두 번째 이후의 실행은 provider thread를 새로 만들지 않고, 그 상태(`idle`)를 유지한 채 `lastRunOrdinal`만 올립니다. 실행이 시작될 때 공유 세션은 `provider-session.updated`로 다시 기록되고(내용은 그대로), `checkpoint-scope.created`는 실행마다 새 `runId`로 다시 기록됩니다. 스레드의 `lastVisitedAt`은 첫 메시지에서만 기록됩니다. (`message-queue`)
- RUN-13. 셸 요약의 `status`와 `latestRun*`은 가장 최근 실행(대기 실행 포함)을 따르고, `activityRun*`과 `activeRunId`는 대기 실행을 건너뛴 가장 최근의 시작된 실행을 따릅니다. 스냅샷의 `messages`는 그 메시지를 가리키는 turn item의 가장 작은 `ordinal` 순서이고, turn item이 없는 메시지(대기 중인 메시지)는 맨 뒤에 놓입니다. (`message-queue`)

## 에이전트 항목

- RUN-14. 노드와 turn item의 id는 codex 항목 id를 `encodeURIComponent`로 감싼 `node:provider:codex:native-item:<id>`, `turn-item:provider:codex:native-item:<id>`입니다. 시작 시각은 `item/started`의 `startedAtMs`이고, `ordinal`은 실행의 범위 안에서 처음 기록된 순서를 따릅니다. (`agent-items`)
- RUN-15. 명령 실행(`commandExecution`)은 `item/started`에서 `tool_call` 노드와 `command_execution` 항목(`input`, `completionWake: "always"`)을 `running`으로 기록하고, `item/completed`에서 다시 기록합니다. 상태는 `inProgress`→`running`, `completed`→`completed`, `failed`→`failed`, `declined`→`cancelled`입니다. 출력과 종료 코드도 저장하지만, 클라이언트에 보내는 형태에서는 `output`을 빼고 출력이 있으면 `outputOmitted: true`, 실패로 보이면 `outputIndicatesFailure: true`를 붙입니다. (`agent-items`, `runtime-requests`)
- RUN-16. 파일 변경(`fileChange`)은 `item/completed`에서만 `file_change` 항목(`fileName`은 첫 파일, `changes`는 `{operation, path}`)으로 기록됩니다. `diffStr`는 저장하지만 실패한 경우를 빼면 클라이언트에 보내지 않습니다. 추론(`reasoning`)은 요약과 본문의 조각마다 `[turnId, itemId, stream, index]`의 JSON을 id로 쓰는 `reasoning` 항목이 되고, 에이전트 메시지처럼 150ms 단위로 묶여 스트리밍됩니다. 스트리밍 중인 에이전트 메시지는 노드와 turn item만 갱신하고, `message.updated`는 완료될 때 한 번 보냅니다. (`agent-items`)

## 승인 요청과 사용자 입력

- RUN-17. codex의 `item/commandExecution/requestApproval`과 `item/fileChange/requestApproval`은 `approval_request` 노드(부모는 해당 항목 노드)와 turn item(`waiting`, `requestKind`, `prompt`), `runtime-request.updated`(`pending`, `responseCapability: live`)가 됩니다. 요청 id는 `runtime-request:provider:codex:provider-turn:<enc>:native-request:<enc>:<uuid>`입니다. `item/tool/requestUserInput`은 해당 항목 노드가 `user_input_request` 노드가 되고, 질문 목록이 turn item에 들어갑니다. 셸 요약의 `pendingRuntimeRequest`는 가장 먼저 생긴 대기 요청의 `{id, kind, createdAt, blocking: true}`입니다. (`runtime-requests`)
- RUN-18. `runtime-request.respond`는 요청을 `resolved`(`decision` 또는 `answers` 포함)로 바꾸고, 노드와 turn item을 `completed`(거절과 취소는 `cancelled`)로 기록한 뒤 codex에 답합니다. 사용자 입력의 turn item에는 `questionAnswer`가 붙고, codex에는 `{<질문 id>: {answers: [...]}}`로 보냅니다. `acceptForSession`이나 `acceptAlways`로 승인하면 같은 codex 스레드의 같은 종류 요청은 묻지 않고 승인합니다. 대기 중이 아닌 요청에는 오류를 돌려줍니다. (`runtime-requests`)

## 체크포인트 파일

- RUN-19. 실행이 끝날 때 기록하는 체크포인트의 `files`는 이전 체크포인트 ref와 새 ref 사이의 `git diff --numstat -z` 결과입니다. 각 파일은 `{path, kind: "modified", additions, deletions}`이고(바이너리는 0), 이름이 바뀐 파일은 새 경로를 쓰며, 경로의 `localeCompare` 순서로 정렬됩니다. 체크포인트 turn item의 `files`도 같은 목록입니다. 두 체크포인트 사이에 HEAD가 움직였다면 턴이 쓴 경로(양쪽 끝의 커밋되지 않은 변경, 그 사이에 만든 커밋의 경로)만 남깁니다. 클라이언트에 보내는 형태에서는 200개까지만 보내고, 넘치면 `filesTotal`(`count`, `additions`, `deletions`)을 붙입니다. (`agent-items`)

## 작업 공간 준비 (`defer_start`)

- RUN-20. `launchThread`가 보내는 첫 메시지는 `dispatchMode: {type: "defer_start", workspaceStrategy}`로 들어갑니다. 실행은 `status: "preparing"`과 `workspacePreparation`(작업 공간 전략)을 갖고, root node의 `checkpointScopeId`는 `null`이며 `checkpoint-scope.created`는 아직 없습니다. 사용자 turn item 바로 다음 `ordinal`에 `turn-item:provider:codex:native-item:<enc("workspace-preparation:"+runId)>` 항목(`type: "command_execution"`, `input`과 `title`은 `"Preparing workspace"`, `running`)이 생깁니다. `launchThread`의 응답은 이 상태를 담습니다. 그다음 `<commandId>:release` 명령이 `checkpoint-scope.created`, scope가 채워진 `node.updated`, 완료된 준비 항목(`title: "Workspace ready"`, `output: "Workspace preparation completed."`, `exitCode: 0`), `run.updated`(`starting`)를 기록하고 턴을 시작합니다. root 전략에서는 준비할 것이 없어 바로 이어집니다. (`title-generation`)

## Claude 프로바이더

- RUN-21. 모델 선택의 `instanceId`가 `claudeAgent`인 스레드는 `claude` CLI를 stream-json 모드로 실행합니다(`--output-format stream-json --verbose --input-format stream-json --permission-prompt-tool=stdio --include-partial-messages --model=<모델> --permission-mode=<모드>`, 첫 실행은 서버가 만든 UUID로 `--session-id`, 이후 새 프로세스는 `--resume`). 권한 모드는 계획 모드면 `plan`, `full-access`면 `bypassPermissions`(`--allow-dangerously-skip-permissions`와 함께), 그 밖에는 `default`입니다. 프로세스는 스레드마다 하나이고 실행 사이에 유지됩니다. provider session id는 `provider-session:provider-instance:claudeAgent:thread:<스레드 id>:<uuid>`이고, provider thread의 `nativeThreadRef`는 그 세션 UUID(`strong`)이며, `nativeMetadata`는 `{title, claudeAttachmentBudget: {bytesUsed, pagesUsed: 0}}`입니다. `bytesUsed`는 세션에 보낸 메시지 텍스트를 JSON 문자열로 쓴 바이트 수의 누적입니다. (`claude-message`, `claude-queue`)
- RUN-22. 턴의 provider turn id는 `provider-turn:provider:claudeAgent:native-turn:<enc("turn:"+attemptId)>`입니다. 턴은 `nativeTurnRef`가 `weak`인 `running`으로 시작하고, 직후에 세션 원인의 `provider-thread.updated`(`active`)가 이어집니다. 스트림의 `message_start`와 `message_delta`마다 `tokenUsage`(`usedTokens`, `maxTokens`, `inputTokens`, `cachedInputTokens`, `outputTokens`, `reasoningOutputTokens: 0`)를 담은 `running` 갱신이 `strong`으로 기록되고, 어시스턴트 메시지에서 모델을 알게 된 뒤에는 `model`도 붙습니다. 자동 압축 창(auto-compact window)이 설정되어 있고 그 값이 `maxTokens`보다 작으면 `autoCompactThreshold`(창 크기에서 33000을 뺀 값)도 붙습니다. 창 크기는 프로젝트의 `.claude/settings.local.json`과 `.claude/settings.json`, 사용자의 Claude `settings.json`에 있는 `env`, 그다음 환경 변수에서 `CLAUDE_CODE_AUTO_COMPACT_WINDOW`를 찾고, 없으면 같은 파일들의 `modelSettings.<모델>.autoCompactWindow`와 `autoCompactWindow`를 봅니다. `DISABLE_COMPACT`나 `DISABLE_AUTO_COMPACT`가 참이거나 `autoCompactEnabled`가 `false`이면 붙지 않습니다. 적합성 실행기는 기록이 컴퓨터 설정에 따라 달라지지 않도록 `CLAUDE_CODE_AUTO_COMPACT_WINDOW=500000`과 빈 `CLAUDE_CONFIG_DIR`을 지정합니다. `result` 메시지에서 턴은 `nativeTurnRef`가 결과 메시지의 `uuid`(`weak`)이고 `turnTokenUsage`가 `complete`인 `completed`가 되며, 마지막 `tokenUsage`도 남습니다. 그다음 세션 원인의 `provider-thread.updated`(`active`)가 오고, 나머지는 codex 실행과 같습니다. 다음 실행부터 provider thread의 `contextUsage`는 이전 턴의 `{usedTokens, maxTokens}`입니다(대기 실행이 시작되는 순간의 갱신에는 아직 없습니다). (`claude-message`, `claude-queue`)
- RUN-23. 어시스턴트 메시지의 각 내용 블록이 항목이 됩니다. 키는 `<API 메시지 id>:<블록 종류>:<블록 index>`입니다. `text`는 codex의 에이전트 메시지와 같은 노드, 메시지, turn item이 되고, `thinking`은 노드 없이 root node에 붙은 `reasoning` turn item(`title: "Thinking"`)이 됩니다. `tool_use`는 도구 이름에 따라 `command_execution`(Bash, `input`은 명령), `file_change`(Write, Edit, MultiEdit, NotebookEdit, `fileName`은 파일 경로), `web_search`(WebSearch, WebFetch), `dynamic_tool`(그 밖, `toolName`과 `input`)이 되고, `tool_result`에서 `completed`(오류면 `failed`)로 끝납니다. 도구 출력은 저장하지만 클라이언트에 보내는 형태에서는 빼고 `outputOmitted: true`를 붙입니다. (`claude-items`, `claude-requests`)
- RUN-24. CLI의 `can_use_tool` 요청은 승인 요청이 됩니다. 노드, `runtime-request.updated`, turn item 순서로 기록되고, 요청 종류는 도구에 따라 `command`, `file-change`, `file-read`이며, `command` 요청의 `responseCapability`에는 `supportsFeedback: true`가 붙습니다. 승인하면 `{behavior: "allow", updatedInput}`, 거절하면 `{behavior: "deny", message: "User declined tool execution."}`으로 답합니다. 거절된 승인 요청의 turn item은 다시 기록되지 않고, 도구 항목은 `failed`로 끝납니다. `AskUserQuestion`의 `can_use_tool`은 질문 텍스트를 질문 id로 쓰는(`multiSelect` 포함) `user_input_request`가 되고, 답은 `updatedInput`의 `answers`(`{<질문 텍스트>: <답>}`)로 보냅니다. 중지는 `interrupt` 제어 요청을 보내고, 결과 메시지가 오면 턴은 `weak`인 원래 `nativeTurnRef`와 `unavailable` 사용량으로 `interrupted`가 되며, 세션 원인의 `provider-thread.updated`는 `idle`입니다. (`claude-requests`, `claude-stop`)

## yatt-server 구현 메모

- v2 이벤트(`run.*`, `node.updated`, `message.updated` 등)는 업스트림이 스레드 스트림에 보내는 봉투 그대로 저장하고, 업스트림의 `applyToProjection`처럼 id 기준 upsert로 접습니다(`server/internal/store/v2.go`). 스냅샷의 목록 순서는 업스트림 스냅샷 쿼리의 `ORDER BY`를 따릅니다.
- codex 프로세스는 프로바이더 인스턴스마다 하나를 공유합니다(`provider-session:provider-instance:codex:shared`). 실행 파일은 `PATH`의 `codex`이고, `YATT_CODEX_BIN`으로 바꿀 수 있습니다.
- Claude CLI는 `PATH`의 `claude`이고, `YATT_CLAUDE_BIN`으로 바꿀 수 있습니다. CLI를 띄운 Claude Code 세션의 환경 변수(`CLAUDECODE`, `CLAUDE_CODE_SESSION_ID` 등)는 넘기지 않습니다. Claude에서 아직 다루지 않는 것: 하위 에이전트와 백그라운드 작업, 계획(ExitPlanMode)과 할 일 목록, 첨부 파일, MCP 서버 등록, 압축(compaction), 사용량 한도와 계정 상태 확인, 실행 중 모델 전환.
- 아직 다루지 않는 것: worktree 작업 공간 전략과 설정 스크립트, 대기열 보류(`queueHeld`)와 중지 후 대기열 처리(yatt-server는 중지되거나 실패한 실행 뒤에도 다음 대기 실행을 시작합니다), 권한 승인(`item/permissions/requestApproval`)과 MCP 도구, 웹 검색, 계획, 하위 에이전트 같은 나머지 항목(codex의 다른 서버 요청은 오류로 응답), 명령 출력 스트림(`subscribeCommandOutput`), HEAD가 움직였을 때 fetch로 들어온 커밋을 턴의 경로에서 빼는 처리, T3 MCP 도구와 추가 지시문(`additionalContext`).
