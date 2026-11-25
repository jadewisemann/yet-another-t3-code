# 스펙 계층

Go 서버(`server/`)가 구현해야 하는 동작을 업스트림 T3 Code 코드와 분리해서 기록합니다. Go 코드는 업스트림 TypeScript를 직접 읽고 옮기는 대신, 이 폴더의 스펙과 `conformance/`의 시나리오를 기준으로 작성합니다.

| 위치 | 작성 방식 | 내용 |
|---|---|---|
| `upstream-version` | 직접 수정 | 스펙과 골든 기록이 기준으로 삼는 업스트림 버전입니다. 지금은 YATT가 번들하는 `0.1.11`입니다. |
| `generated/rpc.json` | 자동 생성 | WebSocket RPC 메서드 전체의 payload, 성공 값, 오류를 JSON Schema(draft 2020-12)로 기록합니다. |
| `generated/rpc-methods.tsv` | 자동 생성 | 메서드 목록과 종류(unary/stream)를 한 줄씩 기록한 색인입니다. diff를 눈으로 확인할 때 사용합니다. |
| `phases.tsv` | 직접 수정 | 메서드마다 어느 포팅 단계(P1~P4)에서 구현하는지 지정합니다. |
| `coverage.md` | 자동 생성 | 단계별 메서드 수, 구현된 메서드, 시나리오가 검증하는 메서드를 보여 줍니다. |
| `protocol/` | 직접 작성 | 프레이밍, URL, 인증처럼 스키마로 표현되지 않는 전송 규칙입니다. |
| `behavior/` | 직접 작성 | 도메인별 동작 규칙입니다. 규칙마다 번호를 붙이고, 그 규칙을 검증하는 시나리오를 연결합니다. |

## 생성 방식

`tools/spec-extract/extract.ts`가 업스트림 체크아웃의 `packages/contracts/src/rpc.ts`에서 `WsRpcGroup`을 불러와서, 메서드마다 Effect Schema를 `Schema.toCodecJson`의 인코딩 형태, 즉 실제로 WebSocket에 오가는 JSON 형태의 JSON Schema로 변환합니다. 같은 모양이 두 번 이상 나오는 큰 하위 스키마는 내용 해시로 이름을 붙여 `$defs`로 옮기므로, 업스트림에서 관련 없는 부분이 바뀌어도 이름이 다시 매겨지지 않습니다.

```sh
scripts/spec-update.sh v0.1.12     # 태그 기준으로 다시 생성하고 바뀐 메서드를 출력
scripts/spec-update.sh nightly     # 가장 최근 nightly 태그와 비교
```

## 알려진 한계

- 브랜드 타입처럼 Effect가 선언(declaration)으로 정의한 일부 필드는 제약 없는 스키마(`{}`)로 나옵니다. 예를 들어 `modelSelection.instanceId`와 `model`이 그렇습니다. 이런 필드는 `behavior/` 문서와 골든 기록으로 보완합니다.
- `Schema.optional` 필드는 `null`도 허용하는 형태로 나옵니다. 업스트림 디코더가 JSON의 `null`을 "없음"으로 받아들이기 때문이며, 서버가 보내는 값은 대부분 키를 생략하거나 `null`을 씁니다.
- HTTP 경로(`/oauth/token`, 첨부 업로드, 미리보기 프록시 등)는 RPC 그룹에 없으므로 이 추출기가 다루지 않습니다. `protocol/http.md`에 직접 정리합니다.
- DB 스키마는 스펙에 포함하지 않습니다. Go 서버는 `~/.t3`와 별개의 데이터 폴더를 쓰므로 업스트림 마이그레이션을 따라갈 필요가 없습니다 (`docs/go-port/PLAN.md` 참고).
