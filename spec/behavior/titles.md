# 스레드 제목 생성 (P2)

`launchThread`에 `generateTitle: true`와 첫 메시지를 보내면 서버가 제목을 만듭니다. 업스트림은 `codex exec`로 제목을 받고, 시나리오는 가짜 codex(`conformance/fixtures/fake-codex.mjs`)가 `Fake title: <메시지>`로 답하게 해서 기록합니다.

- TITLE-1. 첫 메시지를 받을 때 스레드에 메시지가 없고 제목이 일부러 정한 것이 아니면, 서버는 `title`을 시드(`launchThread`의 `title`)로 두고 `titleRegeneration: {requestId, startedAt}`를 기록합니다. `requestId`는 `launchThread`의 `commandId`입니다. 일부러 정한 제목은 사용자가 바꾼 제목(`titleRenamed: true`)이거나, 비어 있지 않고 `New thread`도 아니며 `… thread <숫자>` 꼴의 자리표시도 아니고 시드와도 다른 제목입니다. 이 기록은 첫 방문 기록(`lastVisitedAt`) 다음의 별도 `thread.metadata-updated`입니다. (`title-generation`)
- TITLE-2. 제목은 `codex exec`로 만듭니다. 인자는 `exec`, 플러그인을 끄는 `--config features.connectors=false`, `features.plugins=false`, `features.apps=false`, `--ephemeral --skip-git-repo-check -s read-only --model <모델> --config model_reasoning_effort="low" --config features.codex_hooks=false --config features.hooks=false --output-schema <파일> --output-last-message <파일> -` 순서이고, 프롬프트는 표준 입력으로 보냅니다. 먼저 `codex mcp list --json`으로 켜진 MCP 서버를 확인해서 `mcp_servers` 설정으로 끕니다. 기본 모델은 `gpt-6-luna`이고, 계정의 `model/list`에 없으면 같은 계열이나 기본 모델로 바꿉니다. 출력 스키마는 `{title: string, needsRefinement: boolean}`입니다. (`title-generation`, `server/internal/server/testdata`)
- TITLE-3. 프롬프트는 업스트림 `buildThreadTitlePrompt`의 첫 제목용 문장에 프로젝트 이름을 피하라는 규칙과 `User message:` 뒤의 메시지(8000자를 넘으면 앞뒤만)를 붙인 것이고, 첨부가 있으면 `Attachment metadata:` 목록이 이어집니다. (`server/internal/server/testdata/title-prompt.txt`)
- TITLE-4. 받은 제목은 한 줄로 줄이고 감싼 따옴표와 겹친 공백을 없애며 120자를 넘으면 `...`로 자릅니다. 비면 `New thread`가 되고, 그러면 제목을 바꾸지 않습니다. 실패하면 2초와 4초 간격으로 두 번 더 시도합니다. (`server/internal/server/title_test.go`)
- TITLE-5. 끝나면 `<requestId>:title-complete` 명령이 `titleRegeneration`을 `null`로 되돌리고, 새 제목이 있으면 `title`과 `titleRenamed: false`를 기록합니다. 그사이 다른 요청이 시작됐으면 아무것도 하지 않습니다. 셸 요약은 `titleRegeneration`을 항상 담고, 스레드 레코드는 처음 요청한 뒤부터 담습니다. `thread.metadata.update`로 제목을 바꾸면 `titleRenamed: true`가 됩니다. (`title-generation`)

## yatt-server 구현 메모

- 아직 다루지 않는 것: 다시 만들기(`thread.title.regenerate`)와 `lastTitleRegeneration`, 사용자 제목 지침(`threadTitleInstructions`)과 텍스트 생성 모델 설정, 연결된 PR과 이슈 문맥(`linkedContext`), 포크 스레드의 제목, `needsRefinement`에 따른 후속 처리, Claude 같은 다른 프로바이더로 만드는 제목.
