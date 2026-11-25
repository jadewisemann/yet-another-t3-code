# 적합성 시나리오

`scenarios/*.json`은 서버 구현과 무관한 JSON 시나리오이고, `golden/*.json`은 업스트림 `t3`에서 기록한 정규화된 결과입니다. 러너는 `server/cmd/t3conform`입니다. 전체 방법론은 `docs/go-port/PLAN.md`에 있습니다.

```sh
scripts/conformance.sh record     # 업스트림 t3(spec/upstream-version)로 골든 기록
scripts/conformance.sh upstream   # 업스트림이 골든과 일치하는지 확인 (항상 통과해야 함)
scripts/conformance.sh go         # yatt-server를 골든과 비교 (expected-failures.txt 기준)
```

러너는 시나리오마다 서버를 임시 데이터 폴더와 빈 git 작업 폴더로 새로 띄우고, `auth session issue`로 토큰을 받은 뒤 `/ws`에 접속합니다.

## 시나리오 형식

```json
{
  "description": "무엇을 확인하는지",
  "tier": "p1",
  "steps": [
    { "op": "subscribe", "as": "shell", "method": "orchestration.subscribeShell", "payload": {} },
    { "op": "await", "stream": "shell", "match": { "kind": "snapshot" } },
    { "op": "call", "method": "projects.mutate", "payload": { "projectId": "${uuid:project}", "workspaceRoot": "${workspace}" } },
    { "op": "interrupt", "stream": "shell" },
    { "op": "http", "path": "/.well-known/t3/environment", "record": "keys" }
  ]
}
```

| op | 동작 | 기록 내용 |
|---|---|---|
| `call` | 요청을 보내고 `Exit`를 기다립니다. | `exit` |
| `subscribe` | 스트림 요청을 보냅니다. 값은 백그라운드에서 모입니다. | 없음 |
| `await` | 스트림에서 `match`와 일치하는 값이 올 때까지 기다립니다. `match`는 부분 일치입니다. | 지난 `await` 이후 받은 값부터 일치한 값까지 |
| `interrupt` | 스트림을 끊고 `Exit`를 기다립니다. 남은 값은 기록하지 않습니다. | `exit` |
| `http` | HTTP 요청을 보냅니다. `auth: true`이면 러너의 토큰을, `bearer`가 있으면 그 값을 붙입니다. `body`, `contentType`도 보낼 수 있습니다. | `record`에 따라 상태 코드, 최상위 키, 또는 본문 전체 |
| `cli` | 서버 바이너리를 `args`로 실행합니다. `--base-dir`가 자동으로 붙습니다. | 종료 코드와 출력(`record`에 따라) |
| `file` | 데이터 폴더 아래 파일을 읽습니다. | 파일 내용(`record`에 따라) |
| `connect` | `bearer` 토큰으로 `/ws`를 하나 더 열어 봅니다. | 성공하면 101, 아니면 거절된 상태 코드 |

`call`, `http`, `cli`는 `capture`로 JSON 결과의 문자열 필드를 변수로 저장할 수 있고, 이후 단계에서 `${var:이름}`으로 씁니다. `await`의 `record: "match"`는 일치한 값 하나만 기록하고, `ignore`에 적은 필드는 `<ignored>`로 기록합니다. 업스트림 자체가 실행마다 다르게 보내는 값에만 씁니다.

자리 표시자: `${workspace}`(작업 폴더), `${base}`(데이터 폴더), `${uuid:이름}`(이름별로 고정되는 새 UUID), `${id:이름}`, `${now}`, `${origin}`, `${port}`, `${var:이름}`. 기록할 때는 이 값들이 다시 `<workspace>`, `<uuid:이름>` 같은 표시로 바뀌고, 그 밖의 UUID는 처음 나온 순서대로 `<uuid#1>`, 시각은 `<time>`이 됩니다.

`await` 사이에 오는 값의 묶음 단위(`Chunk` 경계)는 기록하지 않습니다. 실행 타이밍에 따라 달라지기 때문입니다.
