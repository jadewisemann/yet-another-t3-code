# YAT3: Yet Another T3 Code

[T3 Code](https://github.com/pingdotgg/t3code)의 서버는 그대로 사용하고, 화면만 [GPUI](https://www.gpui.rs/)(Zed의 Rust UI 프레임워크)로 다시 만든 네이티브 클라이언트입니다. 에이전트 실행, 스레드 저장, 프로바이더 연결은 모두 T3 Code 서버가 처리하며, 이 앱은 그 서버의 WebSocket RPC에 접속해서 스레드를 보여 주고 명령을 보냅니다.

| 라이트 | 다크 |
|---|---|
| ![light](docs/screenshots/light.png) | ![dark](docs/screenshots/dark.png) |

## 현재 되는 것

- 프로젝트별 스레드 목록 (진행 중인 스레드와 답변을 기다리는 스레드는 점으로 표시)
- 대화 보기와 실시간 스트리밍, 메시지 보내기 (실행 중이면 대기열에 추가)
- 새 스레드 시작 (같은 프로젝트에서 최근에 쓴 모델과 실행 모드를 재사용)
- **스레드 이름 변경**: `F2`, 헤더의 Rename 버튼, 제목이나 목록 항목 더블클릭
- 실행 중지, 스레드 보관
- 단축키와 색상, 글꼴을 설정 파일로 변경 (재시작 없이 다시 불러오기 가능)

아직 없는 것: diff 보기, 터미널, 승인 요청에 응답하기(표시만 함), 첨부 파일, 모델 선택, Markdown 렌더링, 메시지 텍스트 선택. 입력창은 한 줄입니다.

## 설치

### Homebrew (macOS Apple silicon)

```sh
brew tap jadewisemann/yat3 https://github.com/jadewisemann/yet-another-t3-code
brew install --cask yat3
```

업데이트는 `brew upgrade --cask yat3`로 합니다. 설치하면 `/Applications/YAT3.app`과 터미널용 `yat3` 명령이 함께 생깁니다. 앱은 개발자 서명 없이 ad-hoc 서명만 되어 있으므로, cask가 설치 직후 격리 속성(quarantine)을 지워서 바로 열리게 합니다.

새 버전은 `Cargo.toml`의 `version`을 올려 main에 병합하면 배포됩니다. `Release` 워크플로가 macOS 앱을 빌드해 GitHub Release를 만들고, `Casks/yat3.rb`의 버전과 sha256을 자동으로 갱신합니다.

### 직접 빌드

Rust stable과 Xcode Command Line Tools가 있으면 됩니다. (Metal 셰이더를 실행 시점에 컴파일하므로 Xcode 전체는 필요하지 않습니다.)

```sh
cargo build --release
scripts/bundle-macos.sh        # target/release/YAT3.app 생성
```

## 서버와 연결하기

T3 Code 데스크톱 앱이나 `t3` 서버가 실행 중이어야 합니다. 그 상태에서 페어링 링크를 만듭니다.

```sh
npx t3@nightly pair            # 또는 설치된 t3 CLI로 `t3 pair`
```

출력된 `http://127.0.0.1:…/pair#token=…` 링크를 YAT3 첫 화면에 붙여 넣고 Enter를 누르면 연결됩니다. 터미널에서 `yat3 pair <링크>`로 해도 됩니다. 발급된 토큰은 설정 폴더의 `connection.json`에 저장되며 30일 동안 유효합니다. 만료되면 앱이 다시 페어링 화면을 보여 줍니다.

`T3_SERVER_URL`과 `T3_TOKEN` 환경 변수를 지정하면 저장된 연결 대신 그 값을 사용합니다.

## 설정

설정 폴더는 macOS에서 `~/Library/Application Support/yat3`, Linux에서 `~/.config/yat3`입니다 (`YAT3_CONFIG_DIR`로 변경 가능). `Cmd-,`로 폴더를 열 수 있고, 파일을 고친 뒤 `Cmd-Shift-,`를 누르면 다시 불러옵니다.

### 단축키: `keymap.json`

Zed와 같은 형식입니다. 기본값 위에 덧붙이며, `null`로 지정하면 그 키의 기본 동작을 없앱니다. `secondary-`는 macOS에서 `cmd`, 그 밖의 OS에서 `ctrl`을 뜻합니다. 예시는 [examples/keymap.json](examples/keymap.json)에 있습니다.

```json
[
  { "context": "Workspace", "bindings": { "cmd-r": "yat3::RenameThread", "f2": null } }
]
```

| 동작 | 이름 | 기본 키 |
|---|---|---|
| 새 스레드 | `yat3::NewThread` | `cmd-n` |
| 스레드 이름 변경 | `yat3::RenameThread` | `f2`, `cmd-shift-r` |
| 다음 / 이전 스레드 | `yat3::NextThread` / `yat3::PreviousThread` | `cmd-]` / `cmd-[`, `alt-down` / `alt-up` |
| 입력창으로 이동 | `yat3::FocusComposer` | `cmd-l` |
| 사이드바 표시 전환 | `yat3::ToggleSidebar` | `cmd-b` |
| 실행 중지 | `yat3::StopThread` | `cmd-.` |
| 스레드 보관 | `yat3::ArchiveThread` | `cmd-shift-backspace` |
| 단축키 목록 | `yat3::ToggleShortcuts` | `cmd-/` |
| 설정 다시 불러오기 | `yat3::ReloadConfig` | `cmd-shift-,` |
| 설정 폴더 열기 | `yat3::OpenConfigFolder` | `cmd-,` |
| 연결 해제 후 다시 페어링 | `yat3::Disconnect` | (메뉴) |

입력창 안의 편집 키(`text_input::Submit`, `text_input::Cancel` 등)도 `"context": "TextInput"`으로 바꿀 수 있습니다.

### 외형: `theme.json`

기본 팔레트는 Claude 앱처럼 따뜻한 중성색과 테라코타 강조색을 씁니다. 지정한 항목만 덮어씁니다. 예시는 [examples/theme.json](examples/theme.json)에 있습니다.

- `mode`: `auto`(시스템 따라감), `light`, `dark`
- `uiFont`, `messageFont`, `fontSize`: 화면 글꼴, 대화 본문 글꼴(macOS 기본값 Georgia), 기본 크기
- `light`, `dark`: `background`, `sidebar`, `surface`, `border`, `text`, `muted`, `accent`, `accentText`, `selection`, `rowHover`, `rowSelected`, `userBubble`, `danger`, `working` 색상 (`#rgb`, `#rrggbb`, `#rrggbbaa`)

## 구조

| 파일 | 역할 |
|---|---|
| `src/rpc.rs` | Effect RPC 프로토콜 클라이언트 (별도 스레드의 tokio + WebSocket) |
| `src/protocol.rs` | 사용하는 orchestration v2 메시지와 명령 정의 |
| `src/config.rs` | 페어링(토큰 교환)과 연결 정보 저장 |
| `src/workspace.rs` | 메인 화면 |
| `src/keymap.rs` | 액션 정의, 기본 단축키, `keymap.json` 로더 |
| `src/theme.rs` | 팔레트와 `theme.json` 로더 |
| `src/text_input.rs` | IME를 지원하는 한 줄 입력창 (GPUI 예제 기반) |

서버 프로토콜의 기준은 업스트림 `packages/contracts/src/orchestrationV2.ts`와 `rpc.ts`입니다. 서버가 `orchestrationProtocol` 버전을 올리면 접속이 거부되며(HTTP 426), 그때 이 클라이언트를 갱신해야 합니다.

### 실제 서버 없이 화면 확인하기

```sh
pip install websockets==10.4
python3 scripts/fake-server.py &
T3_SERVER_URL=http://127.0.0.1:3999 T3_TOKEN=x cargo run
```

## 라이선스와 출처

`src/text_input.rs`는 GPUI의 `examples/input.rs`(Apache-2.0, Zed Industries)를 바탕으로 수정했습니다. `vendor/xattr`는 xattr 0.2.3(MIT/Apache-2.0)에 최신 libc 호환 수정을 한 줄 더한 사본입니다.
