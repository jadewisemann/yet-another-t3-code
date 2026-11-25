# 첨부 파일

메시지에 이미지와 파일을 붙이는 흐름입니다. 업스트림 `AttachmentUpload`, `attachmentStore`, `providerMessageTextWithAttachmentPaths`와 같은 규칙을 따릅니다. (`attachments`)

- ATT-1. `attachments.createUploadUrl`은 `{attachmentId, relativeUrl, expiresAt}`를 돌려줍니다. `type`은 기본값이 `image`입니다. id는 `pending-<uuid>`이고, 파일이면 확장자를 붙여 `pending-<uuid>-<ext>`가 됩니다. `relativeUrl`은 `/api/attachments/upload/<claims>.<signature>`이며 10분 동안 유효합니다. 서명은 서버만 아는 키로 만든 HMAC입니다.
- ATT-2. 그 URL로 보내는 `POST`에는 bearer 토큰이 필요 없습니다. 본문 길이가 `sizeBytes`와 다르면 400이고, 맞으면 `<base-dir>/userdata/attachments/<id><확장자>`에 저장한 뒤 204를 돌려줍니다. 이미지 확장자는 MIME 형식에서 정하고(`image/png`는 `.png`, `image/jpeg`는 `.jpg`), 파일 확장자는 이름에서 정합니다(알 수 없으면 `.bin`). 같은 URL로 다시 보내면 덮어씁니다.
- ATT-3. `message.dispatch`의 `attachments`에 있는 대기 중인 업로드는 그 스레드로 옮겨집니다. id의 `pending`이 스레드 id에서 만든 이름(소문자, 영숫자와 `-`, `_`만 남김, 최대 80자)으로 바뀌고, 저장된 파일 이름도 함께 바뀝니다. 메시지와 사용자 메시지 항목에는 바뀐 id가 기록됩니다. 업로드되지 않은 id를 보내면 명령이 실패합니다.
- ATT-4. 에이전트에게 보내는 텍스트에는 첨부마다 `\n\n[Attached <type> "<name>" is saved at: <경로>]`가 붙습니다. codex에는 그 텍스트 항목 다음에 PNG, JPEG, GIF, WebP 이미지마다 `{type: "image", url: "data:<mime>;base64,..."}` 항목이 이어집니다. Claude에는 이미지 블록(`source.type: "base64"`)을 먼저 보내고 텍스트 블록을 마지막에 보냅니다.
- ATT-5. `attachments.delete`는 아직 메시지에 쓰이지 않은 대기 중인 업로드만 지우고 `null`을 돌려줍니다.
- 아직 다루지 않는 것: `assets.createUrl`(첨부 미리보기 URL), `assets.persistChatAttachments`(data URL로 보내는 첨부), Claude에 PDF를 문서 블록으로 보내는 처리, 문서에서 텍스트를 뽑아 두는 처리, 캡처한 창 정보, 오래된 대기 업로드 정리.
