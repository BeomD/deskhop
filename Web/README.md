# Transfer (Web) — DeskHop CDC 파일 터널 웹 버전

기존 Go(`Transfer.exe` / `Go/tunnel.go`) 프로그램을 **브라우저에서 그대로** 쓰는 버전입니다.
설치·빌드가 필요 없고, 두 PC 의 브라우저에서 이 페이지를 엽니다(로컬 서버 필요 — 아래 "실행 방법").

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

배포는 최소 3개 파일이면 됩니다:

```
serve.cmd  +  serve.ps1  +  transfer.html   ← 단일 자체완결 페이지(외부 파일 참조 없음)
```

(`index.html`+`app.js`+`ui.js`+`style.css` 를 함께 두면 `/` 는 `index.html` 을 우선 서빙합니다.)

> `transfer.html` 은 `node Web/build-single.mjs` 로 소스에서 생성됩니다(생성물이 저장소에 포함되어 있어
> 오프라인에서는 만들 필요가 없습니다).

### B. Python / Node 가 있을 때

```bash
# 이 폴더에서
python3 -m http.server 9000          # 또는: npx --yes serve -l 9000
# 브라우저에서 http://localhost:9000/
```

### C. HTTPS 호스팅

GitHub Pages 등 HTTPS 로 올리면 로컬 서버 없이도 됩니다. (보드가 웹 서버는 아니므로 페이지는 보드에서
서빙되지 않습니다. 정상 동작 중 보드는 CDC+HID 장치입니다.)

## 사용법

1. 양쪽 PC 에서 `http://localhost:9000/` 을 엽니다. (포트를 바꾸려면 `serve.ps1 -Port 8080` 처럼 지정)
2. **포트 추가…** 로 각 PC 의 **DeskHop CDC COM 포트**를 선택합니다.
3. **저장 폴더 선택** 으로 수신 파일을 저장할 폴더를 고릅니다. **수신은 항상 이 폴더로 기록**되며,
   연결 전에 반드시 선택해야 합니다(Chromium 계열).
4. 양쪽 모두 **CDC 터널 연결** 을 누릅니다. 상대가 늦으면 **연결 대기** 체크.
5. **파일 선택 / 폴더 선택** 후:
   - **터널로 전송**: 즉시 전송
   - **대기열에 추가** → **대기열 전송 시작**: 순서대로 전송
   - **전송 취소**: 중단(수신측 `.part` 보존 → 다음에 이어받기)
6. 수신측은 저장 폴더에 자동 저장됩니다.

## 기능 (Go 버전과 동일)

- 파일 1개 / **폴더 전체(하위 포함)** 전송, 수신측에서 **같은 트리로 재생성**
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
| `index.html` | UI (소스) |
| `app.js` | 프로토콜/시리얼/파일 로직 (Go `Go/tunnel.go` 포팅) |
| `ui.js` | DOM 컨트롤러 (Go `Go/transfer_ui.go` 대응) |
| `style.css` | 스타일 |
| `transfer.html` | **단일 자체완결 페이지**(오프라인 배포용, 자동 생성) |
| `serve.cmd` / `serve.ps1` | 설치 불필요 로컬 서버(Windows 내장 PowerShell) |
| `build-single.mjs` | `transfer.html` 생성 스크립트 |
| `test/loopback.mjs` | Node 루프백 테스트(핸드셰이크/파일/폴더/빈 파일/이어받기) |
| `test/fakefs.mjs` | 인메모리 File System Access 대역 |

## 테스트

브라우저·보드 없이 프로토콜을 검증합니다.

```bash
node Web/test/loopback.mjs
# ALL TESTS PASSED
```

두 `Tunnel` 을 메모리 파이프로 연결해 HELLO 핸드셰이크(늦은 참여 포함), 단일/폴더/빈 파일 전송,
이어받기 성공·불일치 재시작을 검증하고, 수신은 인메모리 가상 폴더에 기록해 무결성을 확인합니다.

## 참고

- 연결이 가끔 실패하던 문제는 펌웨어의 ARQ 재동기화(`rx_synced`)와 호스트 핸드셰이크 보강으로
  수정되었습니다(커밋 `firmware,host: fix CDC tunnel connect wedging…`). 웹 버전도 HELLO 재전송
  400ms·상대 HELLO 수신 시 연결 성립·DTR 150ms 리셋을 동일하게 적용합니다.
- 미러 모드(입력 미러링)가 켜져 있으면 CDC 가 키 주입에 쓰여 터널이 동작하지 않습니다.
