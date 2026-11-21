# Writer — 보드에 꽂힌 USB 메모리로 직접 파일 쓰기

Transfer 의 형제 버전입니다. Transfer 는 **반대편 PC** 로 파일을 넘기지만,
Writer 는 **보드에 꽂은 USB 플래시(메모리 스틱)** 에 바로 기록합니다.
상대편 PC 도, B 보드도, 네트워크도 필요 없습니다.

```
 PC ──USB CDC── Pico (Writer 펌웨어) ──USB 호스트── USB 메모리
      (DW 프로토콜)                    (MSC)         (FAT/exFAT)
```

- PC 앱이 CDC 포트로 `OPEN/DATA/CLOSE` 프레임을 보내면, 보드가 FatFs 로
  스틱에 파일을 만듭니다. PC 쪽에 임시 파일이나 공유 폴더가 필요 없습니다.
- 보드는 **USB 호스트**가 되어야 하므로 PIO-USB(TinyUSB host) 스택을 함께
  씁니다. 이때 USB-A 포트의 5V(VBUS)를 보드가 공급해야 합니다.

## Transfer / Writer 비교

| 항목 | Transfer (`-tags transfer`) | Writer (`-tags writer`) |
|---|---|---|
| 목적지 | 반대편 PC 의 디스크 | 보드에 꽂힌 USB 메모리 |
| 필요한 보드 | A + B 두 대 | 한 대 |
| 전송 경로 | CDC → UART(ARQ) → CDC | CDC → FatFs → MSC |
| 프로토콜 magic | `DH` (릴레이/ARQ) | `DW` (디스크 세션) |
| 미러 모드와 동시 사용 | 해당 없음 | **불가** (아래 참고) |
| 재개(resume) | 자체 메타 파일 | 파일 길이 + CRC(세션 기준) |

## 구성 요소

| 파일 | 설명 |
|---|---|
| `src/usb_disk.c`, `src/include/usb_disk.h` | 펌웨어: TinyUSB MSC 호스트 + FatFs 마운트/파일 I/O |
| `src/diskproto.c`, `src/include/diskproto.h` | 펌웨어: `DW` 프레임 파서/응답 큐 (코어 0 라우팅 + 코어 1 처리) |
| `src/tunnel.c` | CDC 바이트를 `DW` 세션과 터널 릴레이로 분기 |
| `lib/fatfs/*` | FatFs (R0.15 계열), 설정은 `lib/fatfs/ffconf.h` |
| `pico-sdk/lib/tinyusb/src/class/msc/msc_host.c/.h` | MSC 호스트 클래스 (아래 "벤더링 주의" 참고) |
| `3rd/Go/internal/writer/proto.go` | Go: 프레임 인코딩/디코딩(`DW`, CRC32) |
| `3rd/Go/internal/writer/client.go` | Go: `Hello/Stat/Open/PutFile/Close/Abort/Eject` 클라이언트 |
| `3rd/Go/internal/writer/*_test.go` | Go: 가짜 보드 기반 단위 테스트 |
| `3rd/Go/main_writer.go` | Go: Writer 진입점 (GUI + `writer` 서브커맨드 CLI) |
| `3rd/Go/writer_ui.go` | Go: Writer GUI + CLI 구현 |
| `3rd/Go/build-writer.ps1` | Windows 빌드 스크립트 |
| `tests/diskproto/` | 프로토콜 코어 호스트 테스트 (`run.sh`) |

## 펌웨어 빌드

```bash
cmake -B build -DPICO_BOARD=pico
cmake --build build -j
```

관련 CMake 옵션:

| 옵션 | 기본값 | 의미 |
|---|---|---|
| `DH_USB_DISK` | `ON` | Writer 기능(USB 메모리 기록) 포함. `OFF` 면 순수 DeskHop |
| `DH_DISK_EXFAT` | `ON` | exFAT 지원(+~7 KB flash, +~8 KB RAM). 64 GB 이상 스틱은 대부분 exFAT 로 포맷되어 있어서 기본으로 켭니다 |

- `DH_USB_DISK=ON` 이면 `src/usb_disk.c`, `src/diskproto.c`, `lib/fatfs` 가
  자동으로 빌드에 들어갑니다.
- 현재 빌드 실측치(exFAT 포함): **FLASH 188 KB 영역 중 약 105 KB 사용(여유 ~83 KB)
  / RAM 188636 B (71.96 %)**. FLASH 영역은 `misc/memory_map.ld` 의 `__FLASH_LEN`
  (=188k)이고 그 뒤에 64 KB disk image·metadata 가 이어집니다.
  RAM 여유가 빠듯하므로 버퍼를 늘릴 때는 `DP_IN_RING`/`DP_OUT_RING`
  (`src/diskproto.c`)과 `lib/fatfs/ffconf.h` 의 `FF_MAX_SS` 를 먼저 확인하세요.
  exFAT 을 빼면(`-DDH_DISK_EXFAT=OFF`) RAM 이 180812 B (68.97 %)로 돌아갑니다.

### 벤더링 주의 — `msc_host.c/.h`

이 SDK 트리의 `host/usbh.c` 는 `tuh_init()`/`tuh_deinit()` 슬롯이 있는
`usbh_class_driver_t` 를 요구합니다(`bool (*deinit)(void)` 포함). 반면 TinyUSB
0.16.0 의 `msc_host.c` 에는 `msch_deinit()` 이 없어 링크가 깨집니다.
현재 트리에는 **tinyusb `e84c9f3e22` ("implement tuh_init()", 2024-03-22)**
버전의 `msc_host.c`/`msc_host.h` 를 넣어 두었습니다.

- master 계열(`TUH_EPBUF_TYPE_DEF` 사용)은 이 SDK 에서 컴파일되지 않습니다.
- SDK/TinyUSB 를 올릴 때는 `msc_host.c/.h` 도 같은 커밋으로 다시 맞추세요.

## 하드웨어

| 항목 | 요구사항 |
|---|---|
| USB 호스트 | 보드에 USB-A 포트 또는 OTG 포트 + 호스트 모드 |
| VBUS 5V | 스틱에 공급(보드가 공급). 전원 어댑터는 **5V 1.5A 이상** 권장 |
| 데이터 케이블 | PC↔보드는 반드시 데이터 지원 케이블(충전 전용 X) |
| 스틱 포맷 | FAT32 또는 exFAT(기본 빌드에서 둘 다 지원). 파일 1개는 4 GiB 미만 |
| 허브 | 반드시 **자체 전원** 허브. 무전원 허브는 쓰기 중 리셋이 잦습니다 |

## 호스트 앱 빌드

Windows(PowerShell):

```powershell
pwsh ./3rd/Go/build-writer.ps1        # -> Writer.exe
```

직접 빌드:

```bash
go build -tags writer -ldflags "-H=windowsgui" -o Writer.exe .
```

리눅스에서 크로스 컴파일(순수 Go, CGO 불필요):

```bash
GOOS=windows GOARCH=amd64 go build -tags writer -ldflags "-H=windowsgui" -o Writer.exe .
```

빌드 태그 조합:

| 태그 | 실행 파일 | 진입점 |
|---|---|---|
| (없음) | `KeyTyper.exe` | `main_keytyper.go` — 키 주입 GUI |
| `-tags transfer` | `Transfer.exe` | `3rd/Go/main_transfer.go` — CDC 터널 |
| `-tags writer` | `Writer.exe` | `3rd/Go/main_writer.go` — USB 메모리 기록 |

## 사용법 (GUI)

1. **COM 포트** 를 고르고 **[연결]** — 내부적으로 `HELLO` 핸드셰이크를 합니다.
   실패하면 로그에 확인 사항이 찍힙니다(펌웨어/미러 모드/포트 점유).
2. **[상태 새로고침]** — 라벨, 파일시스템, 전체/여유 용량, 쓰기 가능 여부.
3. **[파일 추가] / [폴더 추가]** — 폴더는 하위 구조까지 재귀적으로 목록에
   들어갑니다. 4 GiB 초과 파일은 미리 걸러집니다.
4. **대상 폴더** — 스틱 안의 경로입니다(비우면 루트, 예: `backup/2026`).
   없는 폴더는 보드가 만듭니다.
5. **[전송 시작]** — 파일마다 `OPEN → DATA×N → CLOSE`, 진행률·속도 표시.
   실패/취소된 항목은 목록에 남아 다시 시도할 수 있습니다.
6. **[꺼내기]** — `EJECT`. 스틱을 뽑기 전에 누르세요.

## 사용법 (CLI)

`Writer.exe writer <stat|put|eject>` — 헤드리스 SBC 나 배치 파일용입니다.

```powershell
Writer.exe writer stat  -port COM7
Writer.exe writer put   -port COM7 -dir backup a.zip b.zip
Writer.exe writer put   -port COM7 -resume big.iso
Writer.exe writer eject -port COM7
```

| 플래그 | 의미 |
|---|---|
| `-port` | COM 포트(필수) |
| `-dir` | 스틱 안 대상 폴더 |
| `-resume` | 부분 파일 이어받기 허용 |
| `-chunk` | DATA 청크 크기(512..4096, 기본 4096) |

`WRITER_DEBUG=1` 을 설정하면 클라이언트 추적 로그가 출력됩니다.

## `DW` 프로토콜

```
'D' 'W' | type(1) | len(2 LE) | payload[len] | crc32(4 LE)
```

- `crc32`: type..payload 전체에 대한 CRC-32(IEEE, zlib 과 동일).
- 최대 control payload `DP_CTRL_MAX = 208`, 최대 DATA payload
  `DP_DATA_MAX = 4100`(= 4바이트 오프셋 + 4096 청크).
- 보드 응답은 `uint8_t frame[9+32]` 버퍼로 만들어지므로 **응답 payload 는
  32바이트를 넘을 수 없습니다**. 넘으면 조용히 버려집니다(현재 최대 23바이트).
- 포트를 열고 처음 두 바이트가 `DW` 면 디스크 세션, 그 외는 예전처럼 터널
  릴레이로 처리됩니다.

| 타입 | 값 | payload |
|---|---|---|
| `HELLO` | `0x01` | 없음 → `HELLO_ACK`(version, maxdata, caps, status) |
| `STAT` | `0x02` | 없음 → `STAT_ACK`(status, mounted, fs_type, writable, total_kb, free_kb, label[11]) |
| `OPEN` | `0x04` | flags(1) + expect_size(4) + path + NUL → `OPEN_ACK`(status, resume_off) |
| `DATA` | `0x05` | offset(4) + bytes |
| `CLOSE` | `0x06` | expect_size(4) + expect_crc(4) → `CLOSE_ACK`(status) |
| `ABORT` | `0x07` | 없음 |
| `EJECT` | `0x08` | 없음 |
| 응답 | `0x81..0x88` | `0x8F` = `ERR`(status), `0xFF` = 프레이밍 오류 |

- `DATA_ACK` 는 **8프레임마다**(`DP_ACK_EVERY`), 그리고 오류 시 즉시 옵니다.
- `fs_type` 은 FatFs 의 `FS_*` enum 값 그대로입니다: `1` FAT12, `2` FAT16,
  `3` FAT32, `4` exFAT (`0` = 볼륨 없음). 앱은 이 번호를 그대로 매핑하며,
  한 칸씩 밀리면 FAT32 가 exFAT 으로 표시됩니다.
- `DATA` 의 offset 은 보드가 기대하는 위치와 정확히 같아야 합니다
  (다르면 `DH_DISK_BAD_ARG`).
- 상태 코드: `OK`0, `EIO`1, `NO_MEDIA`2, `NO_SPACE`3, `BAD_PATH`4,
  `READ_ONLY`5, `BUSY`6, `CRC_MISMATCH`7, `NOT_OPEN`8, `BAD_ARG`9,
  `UNSUPPORTED`10.

### 제약 요약

| 항목 | 값 |
|---|---|
| 파일 최대 크기 | 4 GiB − 1 (32비트 오프셋) |
| 경로 최대 길이 | 200바이트 (`DH_DISK_MAX_PATH`), UTF-8, 상대 경로, `/` 구분 |
| 청크 | 512..4096 (기본 4096) |
| ACK 주기 | 8 프레임 |
| 응답 payload | ≤ 32바이트 |

## 알아둘 동작 (중요)

### 1. 유휴 1.5초에 세션이 풀립니다

`src/tunnel.c` 의 `DISK_IDLE_RELEASE_US = 1500000` — 보드는 CDC 수신이
1.5초 이상 끊기면 디스크 세션을 버리고 터널 모드로 돌아갑니다.
그래서 앱은 각 조작 앞에 `STAT`/`HELLO` 를 한 번씩 보내 세션을 되살립니다.
직접 스크립트를 짤 때도 **1.5초 이상 침묵하지 마세요**(또는 `HELLO` 재전송).

### 2. 미러 모드와 동시 사용 불가

디스크 세션은 같은 CDC 포트를 터널 릴레이와 공유합니다. 보드가 미러 모드
(키보드/마우스 미러링)로 동작 중이면 Writer 세션이 끊깁니다. 파일을 옮길
때는 미러 모드를 끄세요.

### 3. 이어받기(resume)의 CRC 는 "세션 바이트"만 검사합니다

`OPEN` 은 항상 세션 CRC 를 초기화하므로, 이어받기로 완료한 파일의 CRC 는
**이번 세션에서 새로 쓴 구간**만 덮습니다. 파일 전체 무결성 검사가 필요하면
이어받기를 쓰지 말고(체크박스 해제) 처음부터 보내세요.

### 4. 진행률의 두 가지 "보낸 바이트"

- `writer.Progress.Sent` = **파일 절대 오프셋**(이어받기면 0 이 아님)
- `writer.FileResult.Sent` = **이번 세션에서 실제로 보낸 바이트 수**

진행률 UI 는 전자(절대 오프셋 / 전체 크기)를 씁니다. 로그 합계를 낼 때는
후자를 쓰세요.

### 5. `EJECT` 는 FAT 언마운트입니다

`EJECT` 는 파일 핸들을 닫고 볼륨을 언마운트합니다. 재사용하려면 다시
마운트되어야 하며(스틱 재삽입 또는 재연결), Windows 탐색기에서 "안전 제거"한
것과 같은 상태로 보는 게 안전합니다.

### 6. 느린 스틱이 보드를 리부트시키지 않도록 (수정됨)

DeskHop 은 core 1 이 500 ms 안에 루프를 한 바퀴 돌지 못하면(`CORE1_HANG_TIMEOUT_US`)
watchdog 을 풀지 않아 **보드가 리부트**됩니다. 그런데 MSC 마운트/디스크 읽기는
core 1 에서 블로킹되므로, 응답이 느린 스틱을 꽂으면 리부트 → USB 재열거 →
호스트의 COM 핸들이 죽어 **"COMx 쓰기 실패: The device does not recognize the
command"** (ERROR_INVALID_FUNCTION) 가 납니다. 재부팅이 없어도 core 1 이 막힌
동안 CDC OUT 이 NAK 되어 같은 증상이 납니다.

그래서 `src/usb_disk.c` 는 이제:

- SCSI 전송 대기 루프(`msc_wait`)와 `f_mount()` 전후에서 `core1_last_loop_pass`
  를 갱신해, 느린 마운트가 "core 1 행"으로 오인되지 않게 합니다.
- 전송 타임아웃을 `MSC_XFER_TIMEOUT_MS = 1000` 으로 낮춰, 죽은 스틱이 CDC 를
  5초(Writer 의 COM 쓰기 타임아웃) 이상 막지 못하게 합니다.

### 7. core 1 스택 오버플로 (수정됨)

`f_mount`/`f_write` 는 core 1 에서 돌고, 그 안의 `disk_read`/`disk_write` 가
`msc_wait()` 로 `tuh_task()` 를 펌프합니다. 그래서 **USB 열거가 FatFs 호출
스택 위에서** 일어날 수 있습니다(마운트 재시도 루프가 도는 동안에는 특히).

SDK 기본 core 1 스택은 `PICO_CORE1_STACK_SIZE = PICO_STACK_SIZE = 0x800`(2 KB)인데,
`-fstack-usage` 실측치는:

| 함수 | 스택 |
|---|---|
| `enumerate_device` (Pico-PIO-USB) | 1240 B |
| `usb_disk_open` | 536 B |
| `dp_task` | 152 B |
| `f_open` | 136 B |
| `tuh_task` / `pio_usb_host_task` | 104 / 88 B |
| `disk_read` | 56 B |

즉 열거가 FatFs 위에 겹치면 `dp_task → usb_disk_open → f_open → disk_read →
tuh_task → pio_usb_host_task → enumerate_device` 가 **2.3 KB** 를 넘겨 2 KB 스택을
밟습니다. 그 결과 하드폴트 → 리셋 → 재열거 → 위와 같은 COM 쓰기 실패입니다.
워치독만 손봐서는 없어지지 않던 이유입니다.

그래서:

- `CMakeLists.txt` 에서 `PICO_CORE1_STACK_SIZE=0x1000`(4 KB)을 정의합니다.
  SCRATCH_X(4 KB)를 스택 전용으로 쓰며 `.scratch_x` 사용량은 0 입니다.
  빌드 맵에서 `.stack1_dummy 0x20040000 0x1000` 으로 확인할 수 있습니다.
- `tuh_msc_umount_cb` 는 이제 FatFs 를 직접 건드리지 않고 `unmount_pending`
  플래그만 세웁니다. 실제 `f_close()`/`f_mount(NULL)` 은 `usb_disk_poll()`
  (core 1 태스크 루프, USB 스택 바깥)에서 수행합니다 — FatFs 재진입 제거.
- 마운트/언마운트 콜백에서도 `core1_last_loop_pass` 를 갱신해, 느린 열거가
  워치독 리셋으로 이어지지 않게 합니다.

## 테스트

```bash
# 펌웨어 프로토콜 코어 (호스트 컴파일, 가짜 플래시)
tests/diskproto/run.sh          # -> 79 checks, 0 failures

# Go 클라이언트 (가짜 보드)
cd 3rd/Go && go test ./internal/writer/
```

`tests/diskproto` 는 `src/diskproto.c` 를 PC 에서 컴파일해 프레임 파싱,
재개, 오류 응답, 재동기화를 확인합니다. `3rd/Go/internal/writer` 테스트는 같은
프로토콜을 Go 쪽에서 왕복시켜 양쪽이 일치하는지(CRC32 니블 테이블 포함)
검사합니다.

## 문제 해결

| 증상 | 확인 |
|---|---|
| `HELLO` 타임아웃 | 보드에 `DH_USB_DISK=ON` 펌웨어인지 / 미러 모드인지 / 다른 앱이 COM 포트를 잡았는지 / COM 포트 번호 |
| `COMx 쓰기 실패: The device does not recognize the command` | 보드가 리부트되어 COM 핸들이 죽은 경우(위 6·7절). 에러 직후 장치 관리자에 COM 포트가 남아 있는지 / 다시 꽂지 않고 [연결]이 되는지 / 포트 번호가 바뀌었는지 확인. 셋 다 "리셋됨"을 뜻하면 전원(VBUS)도 의심하세요 |
| `STAT` 에서 `USB 메모리 없음` | 스틱 장착 여부, 파티션 없는 스틱(MBR 필요), exFAT 스틱 + `-DDH_DISK_EXFAT=OFF` 빌드 |
| `OPEN` → `BAD_PATH` | 경로 200바이트 초과, `..`/`:` 사용, 빈 항목 |
| `OPEN` → `NO_SPACE` | 여유 공간 부족(대상 폴더가 스틱 루트 기준인지 확인) |
| 쓰기 도중 `EIO` | VBUS 전원 부족 또는 무전원 허브 — 전원 있는 허브로 |
| `CLOSE` → `CRC_MISMATCH` | 전송 중 바이트 유실(케이블/전원). 다시 보내면 재개 가능 |
| 파일이 탐색기에 안 보임 | `CLOSE` 실패 상태 확인, `[꺼내기]` 로 언마운트 |