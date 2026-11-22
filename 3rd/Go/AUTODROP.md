# AutoDrop — 브라우저 없이 "파일 1개면 전송, 아니면 Ctrl+V"

`AutoDrop.exe` 는 Windows 트레이 없는 상주 프로그램이다. 클립보드를 지켜보다가
**파일이 정확히 1개** 들어 있으면 그 파일을 보드(USB 메모리 모드)로 보내고,
그 밖의 모든 경우(문자열 · 빈 클립보드 · 파일 2개 이상 · 폴더)는 **평소처럼
`Ctrl+V`** 로 흘려보낸다.

`3rd/Web` 페이지는 이 프로그램과 아무 관계가 없다. 브라우저도, 웹 서버도,
`localhost:9000` 도 필요 없다 — 그냥 exe 하나.

## 동작 규칙

| 클립보드 내용 | 트리거(`Ctrl+Alt+V`)를 누르면 |
| --- | --- |
| 파일 **1개** | 보드 메모리로 전송 (평소 `Ctrl+V` 는 **그대로** 동작) |
| 파일 2개 이상 | 일반 붙여넣기 |
| 폴더 1개 | 일반 붙여넣기 |
| 문자열만 | 일반 붙여넣기 |
| 빈 클립보드 | 일반 붙여넣기 |

핵심은 두 가지다.

* **정확히 1개**만 본다. 탐색기에서 여러 개를 복사했거나 폴더를 복사했으면
  손대지 않는다. 폴더는 `CF_HDROP` 에 "항목 1개"로 들어오기 때문에 개수만
  세면 폴더를 파일로 오판한다 — 그래서 `FILE_ATTRIBUTE_DIRECTORY` 를 따로
  확인한다(`internal/autodrop.Decide`).
* 트리거는 **`Ctrl+Alt+V`** 다. 기본값이 `Ctrl+V` 가 아닌 이유는, 탐색기 안에서
  파일을 그냥 붙여넣고 싶을 때 방해하지 않기 위해서다. 트리거가 `Ctrl+V` 와
  다르면 에이전트가 보조키를 정리하고 `Ctrl+V` 를 대신 주입한다.

전송에 성공하면 원래의 `Ctrl+V` 는 삼켜진다(파일이라 붙여넣을 대상이 없다).
전송이 실패하면 `paste_on_fail` 이 켜져 있는 동안은 `Ctrl+V` 로 대신 붙여넣는다.

## 빌드

```powershell
cd 3rd/Go
./build-autodrop.ps1        # = go build -tags autodrop -ldflags "-H=windowsgui" -o AutoDrop.exe .
```

`-tags autodrop` 은 진입점을 `main_autodrop.go` 로 바꾼다(`main_keytyper.go`,
`main_writer.go`, `main_transfer.go` 와 같은 방식). 시리얼 계층은 `KeyTyper.go`
의 `openCDC`/`listComPorts` 를, 프로토콜은 `internal/writer`(DW)를 그대로
재사용하므로 `Transfer.exe`/`Writer.exe` 와 같은 펌웨어 위에서 돈다.
**펌웨어는 손대지 않아도 된다.**

## 설정 — `autodrop.json`

`AutoDrop.exe` 옆에 둔다. 파일이 없으면 기본값으로 만들어 준다. 적은 항목만
덮어쓰고 나머지는 기본값을 쓴다.

```json
{
  "port": "",
  "dest": "DROP",
  "timestamp": true,
  "ctrl": true,
  "alt": true,
  "shift": false,
  "paste_on_fail": true,
  "resume": false,
  "poll_ms": 120,
  "skip_explorer": false
}
```

| 키 | 뜻 |
| --- | --- |
| `port` | COM 포트(`"COM7"`). 비우면 자동 — 포트가 정확히 1개일 때만 성공하고, 여러 개면 로그에 목록을 남기고 실패한다. |
| `dest` | 보드 메모리 안 폴더. 기본 `DROP`. `:` 와 `.`/`..` 는 거부한다. |
| `timestamp` | 파일명 앞에 `20261122-140506_` 를 붙인다. 같은 이름을 덮어쓰지 않게 하려면 켜 둔다. |
| `ctrl`/`alt`/`shift` | 트리거 조합. 기본 `Ctrl+Alt+V`. |
| `paste_on_fail` | 전송 실패 시 `Ctrl+V` 로 대체. |
| `resume` | 이어받기. 보드는 크기만 비교하므로 **같은 파일을 다시 보낼 때만** 켜는 것이 안전하다(기본 꺼짐). |
| `poll_ms` | 클립보드 폴링 주기(최소 30). 시퀀스 번호가 바뀔 때만 실제로 읽는다. |
| `skip_explorer` | 탐색기 창이 앞에 있으면 가로채지 않는다. |

트리거를 `Ctrl+V` 자체로 쓰고 싶으면 `alt`/`shift` 를 `false` 로 두면 된다.
그때는 에이전트가 키를 주입하지 않고 판정만 한다(파일 1개면 전송·삼킴, 아니면
OS 기본 동작).

## 실행

```
AutoDrop.exe              상주. Ctrl+Alt+V 를 기다린다. 로그는 autodrop.log
AutoDrop.exe -list        COM 포트 목록만 출력
AutoDrop.exe -send FILE   파일 하나를 보내고 종료 (보드/설정 확인용)
AutoDrop.exe -port COM7   설정보다 우선해서 포트 지정
AutoDrop.exe -dest INBOX  설정보다 우선해서 대상 폴더 지정
```

로그(`autodrop.log`)에 남는 것: 트리거 판정 이유(`일반 Ctrl+V: 문자열`),
전송 진행률(25% 단위), 실패 원인, 보드 핸드셰이크 결과.

## 확인 순서 (처음 한 번)

1. 보드를 USB 메모리 모드로 (펌웨어가 `DH_USB_DISK=ON` 인 빌드).
2. `AutoDrop.exe -list` → 포트가 하나만 보이는지.
3. 탐색기에서 아무 파일 하나를 `AutoDrop.exe` 위로 끌어다 놓지 말고,
   `AutoDrop.exe -send 파일.pdf -dest DROP` 으로 전송이 되는지 먼저 확인.
   (파일 하나만 넣어도 되고, `-send` 가 제일 빠르다.)
4. `AutoDrop.exe` 상주 → 탐색기에서 파일 1개 복사 → `Ctrl+Alt+V` → 보드 메모리에
   `DROP/…_파일.pdf` 가 생기는지.
5. 문자열 복사 후 `Ctrl+Alt+V` → 메모장에 그대로 붙는지. 파일 2개 복사 후
   `Ctrl+Alt+V` → 붙여넣기가 되는지.

`Transfer.exe`/터미널/웹 페이지가 같은 COM 포트를 잡고 있으면 전송이 실패한다
(포트는 배타 점유). AutoDrop 은 전송할 때만 포트를 열고 바로 닫는다.

## 구조

```
autodrop.go            Win32 훅·클립보드·전송 (빌드 태그 autodrop)
main_autodrop.go       진입점, -list / -send
internal/autodrop/     판정 규칙 + 경로 변환 (플랫폼 독립, 리눅스에서 테스트)
  rule.go              Decide / SanitizeName / NormalizeDest / DestPath
  rule_test.go         규칙·경로 테스트
```

* 키보드 훅(`WH_KEYBOARD_LL`)은 **판정만** 한다. 클립보드를 훅 안에서 열면
  데드라인(300ms)에 걸려 입력이 밀리므로, 클립보드는 별도 고루틴이 폴링하며
  시퀀스 번호로 캐시한다.
* 전송은 고루틴에서 한 번에 하나만(`busy`). 같은 클립보드로 두 번 보내지 않도록
  시퀀스 번호를 기억한다(`sentSeq`).

## 테스트

```bash
cd 3rd/Go
go test ./internal/autodrop     # 판정 규칙·경로 (리눅스/맥에서도 돈다)
GOOS=windows GOARCH=amd64 go build -tags autodrop -o /tmp/AutoDrop.exe .
```
