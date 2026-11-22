# Transfer (Web) — DeskHop CDC 파일 터널 웹 버전

기존 Go(`Transfer.exe` / `3rd/Go/tunnel.go`) 프로그램을 **브라우저에서 그대로** 쓰는 버전입니다.
설치·빌드가 필요 없고, 두 PC 의 브라우저에서 이 페이지를 엽니다(로컬 서버 필요 — 아래 "실행 방법").

> **`3rd/Go` 는 더 이상 쓰지 않습니다** — 이후 유지보수는 이 웹 버전(`3rd/Web`)에서만 진행합니다.

```
 PC A <--USB CDC--> Pico A <--UART(ARQ)--> Pico B <--USB CDC--> PC B
        (바이트 파이프)                        (바이트 파이프)
```

- 하위 통신은 **Web Serial API**(`navigator.serial`)로 DeskHop CDC(COM) 포트를 열고,
- 그 위에 Go 버전과 **동일한 앱 프레임**(`"DH" | type | flags | len(4LE) | payload | crc32(4LE)`)을
  올려 파일/폴더/이어받기를 처리합니다.
- 펌웨어는 수정할 필요가 없습니다(기존 CDC 터널 펌웨어 그대로).

## 요구 사항

| 항목 | 내용 |
|---|---|
| 브라우저 | **Chrome / Edge / Opera** (Web Serial) · Firefox 151+ 는 송신 가능 · Safari ❌ |
| 수신 | **File System Access**(저장 폴더 선택) 필요 → **Chromium 계열 전용**(Chrome/Edge/Opera) |
| OS | Windows 11 · macOS · Linux 데스크톱 |
| 컨텍스트 | **보안 컨텍스트 필요** → HTTPS 또는 `http://localhost` (**`file://` 차단됨**) |
| 펌웨어 | 기존 CDC 터널 펌웨어(수정 불필요). Writer(`DH_USB_DISK`) 기본 빌드도 OK |
| 주의 | **COM 포트는 배타 점유** — `Transfer.exe`/`KeyTyper.exe`/터미널이 잡고 있으면 브라우저에서 열 수 없음 |

> Windows 11 은 Edge/Chrome 을 쓰면 됩니다. DeskHop CDC 는 usbser.sys 로 COM 포트로 잡히고
> Web Serial 이 그 포트를 그대로 엽니다.

## 실행 방법

Web Serial 은 **보안 컨텍스트(HTTPS 또는 `http://localhost`)에서만** 동작합니다.
**`file://`(html 을 더블클릭)로는 브라우저가 시리얼 접근을 차단**하므로 반드시 로컬 서버를 씁니다.

### A. 설치 불필요 (권장 · 폐쇄망/오프라인) — Windows 내장 PowerShell

Python/Node 없이, Windows 에 기본 내장된 PowerShell 만으로 로컬 서버를 띄웁니다.

- **`serve.cmd` 를 더블클릭**하면 끝. (내부적으로 `serve.ps1` 을 실행정책 우회로 실행)
- 같은 폴더를 `http://localhost:9000/` 로 서빙하고 **Edge/Chrome 을 자동으로 엽니다.**
- 관리자 권한 불필요(tcp 직접 바인딩). 창을 닫거나 `Ctrl+C` 로 종료.

**`serve` 는 COM 포트를 찾는 역할이 아닙니다.** COM 포트(DeskHop CDC)는 브라우저의 Web Serial 권한으로
직접 열리고, `serve` 는 그 API 가 요구하는 **`http://localhost` 보안 컨텍스트**만 제공합니다.
따라서 **PC 2대 각각 자기 자신의 `localhost` 서버**가 필요합니다(상대 PC 의 서버에 접속하는 구조가 아님).
매번 켜기 번거로우면: **`install-autostart.cmd` 더블클릭** → 로그인 시 `serve.ps1` 이 창 없이 자동 실행됩니다.
해제는 `install-autostart.cmd remove` (관리자 권한 불필요, 사용자 Startup 폴더만 사용).

배포는 최소 3개 파일이면 됩니다:

```
serve.cmd  +  serve.ps1  +  transfer.html   ← 단일 자체완결 페이지(외부 파일 참조 없음)
```

(자동 시작까지 등록하려면 `install-autostart.cmd` + `install-autostart.ps1` 을 더합니다.)

> 이미 서버가 떠 있는 상태에서 `serve.cmd` 를 또 실행해도 안전합니다 — 포트가 사용 중이면 기존 서버를
> 그대로 쓰고 브라우저만 엽니다.

`transfer.html` **하나가 소스이자 배포본**입니다 — 빌드 단계도, 별도 `.css`/`.js` 파일도 없습니다.
`serve.ps1` 은 `/` 요청을 `transfer.html` 로 연결합니다.

> 수정할 때도 `transfer.html` 을 직접 고치면 그대로 반영됩니다. (예전 `index.html`+`style.css`+`app.js`+`ui.js`
> 4파일 + `build-single.mjs` 생성 구조는 같은 내용이 두 벌 존재해 한쪽만 고치는 사고가 생길 수 있어 폐지했습니다.)

### B. Python / Node 가 있을 때

```bash
# 이 폴더에서
python3 -m http.server 9000          # 또는: npx --yes serve -l 9000
# 브라우저에서 http://localhost:9000/transfer.html
# (이 경우 `/` 는 파일 목록이므로 파일명까지 지정. serve.ps1 은 `/` 로 바로 연결해 줍니다)
```

### C. HTTPS 호스팅

GitHub Pages 등 HTTPS 로 올리면 로컬 서버 없이도 됩니다. (보드가 웹 서버는 아니므로 페이지는 보드에서
서빙되지 않습니다. 정상 동작 중 보드는 CDC+HID 장치입니다.)

## 사용법

처음 **한 번만** 아래 2개를 하면, 그 다음부터는 페이지를 열기만 해도 **자동으로 연결**됩니다.

1. 양쪽 PC 에서 `http://localhost:9000/` 을 엽니다. (포트를 바꾸려면 `serve.ps1 -Port 8080` 처럼 지정)
2. **한 번만**: **포트 추가…** → 목록에 **DeskHop 보드만** 뜹니다(USB ID 필터) → 선택.
   목록이 비어 있으면 USB 연결을 확인하거나 **Shift+클릭** 으로 전체 포트를 보세요.
3. **한 번만**: **저장 폴더 선택** → 수신 파일을 저장할 폴더 지정.
   **수신은 항상 이 폴더로 기록**되며(Chromium 계열), 연결 전에 반드시 필요합니다.
4. 이후에는 **자동 연결**(기본 켜짐)이 위 두 가지를 기억해 알아서 터널을 엽니다.
   상대편이 늦어도 **연결 대기** 로 계속 기다리고, 끊기면 자동으로 다시 연결합니다.
   수동으로 **CDC 터널 해제** 를 누르면 자동 연결이 꺼집니다(원하면 체크를 다시 켜면 됨).
5. **파일 선택 / 폴더 선택** 후:
   - **터널로 전송**: 즉시 전송
   - **대기열에 추가** → **대기열 전송 시작**: 순서대로 전송
   - **전송 취소**: 중단(수신측 `.part` 보존 → 다음에 이어받기)
6. 수신측은 저장 폴더에 자동 저장됩니다.

## 자동 연결 · 포트 선택

| 항목 | 동작 |
|---|---|
| **자동 연결** 체크(기본 켬) | 페이지를 열면 포트 자동 선택 + 저장 폴더 복원 + 터널 시작(`connectWait` 로 상대 무한 대기) |
| 포트 자동 선택 | 권한이 남아 있는 포트 중 **DeskHop(0x1209:0xc000 / 0x2e8a:0x107c)** 우선 |
| **포트 추가…** | 선택 창을 **DeskHop 보드로 필터**(다른 장치 안 보임) · **Shift+클릭 = 전체 포트** |
| 저장 폴더 기억 | 핸들을 IndexedDB(`dhweb`)에 저장 → 다음 방문에 권한이 남아 있으면 클릭 없이 복원 |
| 연결 끊김 | 자동 연결이 켜져 있으면 포트 재조회 후 자동 재연결(4–15초 간격, 최대 20회) |
| 수동 **CDC 터널 해제** | 자동 연결을 함께 끔(사용자 의도 존중) |

**브라우저 정책상 클릭이 반드시 필요한 경우**가 두 가지 있습니다.

- **최초 1회**: `navigator.serial.requestPort()` 와 `showDirectoryPicker()` 는 사용자 제스처(클릭) 없이는
  호출할 수 없습니다. 권한이 한 번 부여되면 이후에는 `getPorts()` 로 자동 복원됩니다.
- **저장 폴더 권한 만료**: 권한이 `prompt` 로 돌아가면 조용한 복원이 불가능해, 다시 **저장 폴더 선택**
  을 눌러야 합니다(로그에 안내가 나옵니다). Chrome 은 사이트를 다시 방문할 때 이 핸들 권한을
  대개 유지하지만, 시크릿 창·사이트 데이터 삭제 후에는 초기화됩니다.

> `autoConnect` 를 끄고 쓰려면 브라우저에 저장된 선호(`localStorage: dhweb.autoConnect`)를 따르므로,
> 체크 해제 상태가 다음 방문에도 유지됩니다.

## 붙여넣기 · 드롭 규칙 — **파일이 1개일 때만 전송**

이 창에 **파일 1개를 드롭**하거나 **Ctrl+V** 하면 그 파일을 곧바로 터널 전송으로 넘깁니다.
그 외는 **가로채지 않고 브라우저 기본 동작(일반 붙여넣기)** 에 그대로 맡깁니다.

| 클립보드 / 드롭 내용 | 동작 |
|---|---|
| **파일 정확히 1개** | 현재 전송 대상으로 잡고 즉시 전송(미연결이면 대상만 설정) |
| 파일 0개 (문자열·이미지 텍스트) | 아무 것도 하지 않음 → 일반 Ctrl+V |
| 파일 2개 이상 | 무시하고 로그만 남김 → 일반 동작 |
| 폴더 | 대상 아님(무시) → 일반 동작 |

- 판정은 `DataTransfer`/`ClipboardData` 의 `files.length === 1` 이고, `items[0].kind === 'file'` 이며
  `webkitGetAsEntry().isDirectory` 가 아닐 때만 성립합니다.
- 여러 파일·폴더는 기존처럼 **파일 선택 / 폴더 선택 → 대기열** 로 보내면 됩니다(수동 경로는 그대로).

## 기능 (Go 버전과 동일)

- **파일 1개 드롭/Ctrl+V 즉시 전송**, 파일 1개 / **폴더 전체(하위 포함)** 전송(수동 선택), 수신측에서 **같은 트리로 재생성**
- **대기열**(여러 파일·폴더 순차), **전송 취소**
- **이어받기(resume)**: 수신측 `.part` 의 정렬된 prefix CRC 를 `META_ACK` 로 제안 → 송신측이
  자기 파일의 같은 구간 CRC 와 일치할 때만 그 지점부터 재개
- **전이중**(송신/수신 독립), 진행률(전송/수신 별도)
- 역할(A/B) 배너 표시

## 파일 저장 방식

수신은 **항상 저장 폴더(File System Access)로만** 기록합니다. 다운로드 폴백은 없습니다.

- 저장 폴더는 **연결 전에 선택**해야 하며, 그 폴더에 직접 파일을 씁니다(브라우저 다운로드 목록에 안 남음).
- 폴더 구조 재생성과 **이어받기(.part + `move()`)** 를 지원합니다.
- `showDirectoryPicker()` 는 **Chromium 계열(Chrome/Edge/Opera)** 에만 있습니다. Firefox 는 **송신만** 가능하고
  수신은 불가하므로, 수신이 필요하면 Chromium 계열을 사용하세요.

## Go 버전과의 차이

| 항목 | Go(Transfer) | Web(웹) |
|---|---|---|
| 설치 | .exe 빌드/배포 | 없음(정적 페이지) |
| OS | Windows | Win/mac/Linux(브라우저) |
| CLI | `Transfer.exe tunnel send/recv` | 없음(GUI만) |
| 저장 폴더 | OS 폴더 자동(Downloads) | 사용자가 선택(연결 전 필수) |

## 파일 구성

| 파일 | 설명 |
|---|---|
| `transfer.html` | **단일 소스 + 배포본.** 마크업 + 인라인 `<style>` + 인라인 `<script>` 2개(① 프로토콜·시리얼·파일 전송, ② DOM 컨트롤러) |
| `serve.cmd` / `serve.ps1` | 설치 불필요 로컬 서버(Windows 내장 PowerShell) · `/` → `transfer.html` · `-Port`/`-NoBrowser` |
| `install-autostart.cmd` / `.ps1` | 로그인 시 `serve.ps1` 자동 시작 등록/해제(창 없음) |
| `test/load-source.mjs` | `transfer.html` 에서 마크업/스타일/스크립트 블록을 추출하는 공용 로더 |
| `test/loopback.mjs` | Node 루프백 테스트(핸드셰이크/파일/폴더/빈 파일/이어받기) |
| `test/fakefs.mjs` | 인메모리 File System Access 대역 |
| `test/uismoke.mjs` | UI 초기화 스모크 테스트(가짜 DOM, 포트 0개) |

## 테스트

브라우저·보드 없이 프로토콜을 검증합니다.

```bash
node 3rd/Web/test/loopback.mjs   # 프로토콜(핸드셰이크/파일/폴더/이어받기)
node 3rd/Web/test/uismoke.mjs    # UI 초기화 경로(DOM 대역 + Web Serial 스텁)
# ALL TESTS PASSED / UI SMOKE OK
```

두 테스트 모두 `test/load-source.mjs` 를 통해 `transfer.html` 에서 스크립트 블록을 꺼내 실행합니다
(소스가 1개 파일이므로 테스트도 파일 1개만 봅니다). `uismoke.mjs` 는 가짜 DOM 과 "포트 0개" Web Serial
스텁으로 ①+② 블록을 `vm` 에서 그대로 실행해, 페이지 로드 시 `init()` 이 예외 없이 끝나는지(자동 연결이
조용히 실패하지 않는지) + 외부 참조가 남아 있지 않은지까지 검사합니다. 보드·브라우저 없이 자동 연결
경로의 회귀를 잡기 위한 용도입니다.

두 `Tunnel` 을 메모리 파이프로 연결해 HELLO 핸드셰이크(늦은 참여 포함), 단일/폴더/빈 파일 전송,
이어받기 성공·불일치 재시작을 검증하고, 수신은 인메모리 가상 폴더에 기록해 무결성을 확인합니다.

## 참고

- 연결이 가끔 실패하던 문제는 펌웨어의 ARQ 재동기화(`rx_synced`)와 호스트 핸드셰이크 보강으로
  수정되었습니다(커밋 `firmware,host: fix CDC tunnel connect wedging…`). 웹 버전도 HELLO 재전송
  400ms·상대 HELLO 수신 시 연결 성립·DTR 150ms 리셋을 동일하게 적용합니다.
- 미러 모드(입력 미러링)가 켜져 있으면 CDC 가 키 주입에 쓰여 터널이 동작하지 않습니다.
