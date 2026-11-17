# DeskHop을 SBC 1대로 대체할 수 있을까? (OTG 포트 2개 가정)

> 대상: 이 저장소(`/home/user/deskhop`, fw 0.80 + 포크 기능: CDC 미러/터널)
> 결론 요약: **기술적으로는 가능하지만 "이식"이 아니라 "USB 계층 재작성"이고,
> 진짜 병목은 OTG 포트 개수가 아니라 ① 독립 UDC 2개 ② 지연/지터 ③ 절연·전원 ④ 단일 장애점/보안모델이다.**

---

## 1. 지금 왜 2보드인가 (코드 기준)

보드 1장이 하는 일:

| 방향 | 구현 | 근거 |
|---|---|---|
| PC 쪽 (USB **device**) | RP2040 네이티브 USB + TinyUSB (HID 2~6 인터페이스, 벤더 설정 IF, MSC, CDC) | `src/usb_descriptors.c` |
| 입력기 쪽 (USB **host**) | PIO USB, D+ = GP14 / D- = GP15 | `src/include/pinout.h` |
| 반대편 보드 | UART + 절연(ISO7721/ADuM1201), GP12/13(A), GP16/17(B) | `src/uart.c`, `pinout.h` |

RP2040은 USB 컨트롤러가 **1개**뿐이라, PIO로 두 번째 포트를 만드는 Pico-PIO-USB를 써서
"device 1 + host 1"을 한 보드에 넣었다. 그런데 이 프로젝트가 필요한 건 **PC 쪽 USB device 포트 2개**다.
그래서 PC마다 보드 1장 = 2장 구조다.

두 보드는 UART로 **상태를 복제**해서 하나처럼 동작한다:

- 12바이트 고정 프레임(`packet.h`, 터널만 36바이트), 패킷 타입 27종(`protocol.h`)
- 공유 상태: `active_output`, `switch_lock`, `gaming_mode`, `mirror_mode`, 보더 캘리브레이션,
  키보드 LED 상태, 스크린세이버
- 역할 구분: `board_role`, `CURRENT_BOARD_IS_ACTIVE_OUTPUT` (`constants.h`, `pinout.h`)
- 부가로 UART를 데이터 파이프로 쓰는 기능: 터널(ARQ), 펌웨어 업그레이드, CDC 키 주입

즉 2보드 구조의 본질은 두 가지다.
1. **USB device 포트 2개**가 필요하다 (하드웨어 제약)
2. **두 PC 사이에 전기적 경로를 만들지 않는다** (2kV 절연, 보안 모델)

SBC 1대는 1번을 "OTG 2개"로 해결할 수 있고, 2번은 **오히려 나빠진다** (아래 4절).

---

## 2. 코드 재사용 관점: 무엇이 살아남고 무엇이 죽는가

총 7,869줄(`src/**.c/h`) 기준 대략적인 정리.

### 2.1 SBC에서 그대로 버려지는 것 (약 4,000줄+)

| 파일 | 줄 수 | SBC에서의 대체 |
|---|---|---|
| `uart.c`, `dma.h`, `protocol.c`, `include/packet.h` | ~400 | 보드 간 링크 자체가 없음 → 삭제 |
| `usb_disk.c`, `diskproto.c`, `ramdisk.c`, `usb_descriptors.c`(디스크립터 조립부) | ~1,900 | 커널 gadget(configfs) + 평범한 HTTP 설정 서버 |
| `usb.c`, `hid_parser.c/h`, `hid_report.c` | ~1,000 | 리눅스 커널 HID 스택 (hidraw) |
| 플래시/부트로더/워치독/태스크 스케줄러(`flash.h`, `setup.c`, `tasks.c`, `watchdog.h`) | ~600 | OS가 대체 |
| `handlers.c` 중 `send_value()` 동기화부 | ~200 | 삭제 (원격 상태가 없음) |

**주목**: `diskproto/ramdisk/usb_disk`(1,900줄)는 "12KB 저장공간이 없어서 MSC 가상드라이브로
설정 페이지를 배포"하는 트릭이다. SBC에서는 그냥 `localhost:8080`이면 되므로 통째로 사라진다.
WebHID도, udev 규칙도, "크롬에서만 동작" 제약도 없어진다. `webconfig/`는 그대로 재사용.

### 2.2 살아남는 것 (약 1,300~1,600줄)

| 파일 | 줄 수 | 비고 |
|---|---|---|
| `keyboard.c` (핫키 매칭, LED 처리) | ~380 | 리포트 구조체만 맞추면 거의 무수정 |
| `mouse.c` (절대좌표 누적, 가속, 보더 점프, 임계값) | ~423 | **핵심 로직**, 하드웨어 의존 거의 없음 |
| `handlers.c` 핫키 액션 | ~200 | 스위치/락/게이밍/미러/보더 저장 |
| `constants.c`, `defaults.c`, `config.c` | ~120 | 설정 스키마 (JSON으로 바꿔서 재사용) |
| `usb_descriptors.h`의 **HID 리포트 디스크립터 매크로** | - | `TUD_HID_REPORT_DESC_ABS_MOUSE` 등을 2진 blob으로 추출해 configfs `report_desc`에 그대로 사용 |
| `led.c`(깜빡임), 스크린세이버 | ~100 | 그대로 |

디스크립터를 **바이트 단위로 그대로** 쓸 수 있다는 점이 크다. Windows/macOS의 절대좌표·다중모니터
동작(그리고 그 문제점)이 동일하게 유지된다.

### 2.3 사라지지만 "기능은 더 쉬워지는" 것

- **터널(CDC↔UART ARQ)**: SBC 안에서 두 `hidg`/CDC 사이 in-process 파이프 → ARQ·시퀀스·크레딧 불필요
- **미러 모드**: 두 출력에 같은 리포트를 그냥 2번 write
- **펌웨어 업그레이드(보드 간 4000Hz 전송)**: `apt upgrade` / OTA

---

## 3. 구조 스케치

```
                       ┌──────────────── SBC (Linux) ────────────────┐
 USB KBD/MOUSE ──host──▶ hidraw reader (SCHED_FIFO, busy poll)       │
                       │        │                                    │
                       │        ▼                                    │
                       │   router (active_output, pointer_x/y,       │
                       │           hotkey FSM, border crossing)      │
                       │        │                    │               │
                       │   /dev/hidg0            /dev/hidg1          │
                       │   gadget0 → UDC0        gadget1 → UDC1      │
                       └────────┼────────────────────┼───────────────┘
                          [ISOUSB211]           [ISOUSB211]
                                │                    │
                              PC A                 PC B
```

### 3.1 gadget 준비 (UDC 2개)

```bash
ls /sys/class/udc          # ← 여기에 2개 이상이 보여야 이 설계가 성립
```

```bash
# PC A 쪽 gadget (UDC 이름은 보드마다 다름)
G=/sys/kernel/config/usb_gadget/pcA
mkdir -p $G && cd $G
echo 0x1209 > idVendor; echo 0xc000 > idProduct
mkdir -p strings/0x409 && echo "DeskHop-A" > strings/0x409/product
mkdir -p configs/c.1
# HID 함수 1: 키보드 + 절대마우스 + 컨슈머 + 시스템 (리포트 ID 1~4, DeskHop과 동일)
mkdir -p functions/hid.usb0
echo 1 > functions/hid.usb0/protocol        # 1 = keyboard boot protocol
echo 0 > functions/hid.usb0/subclass
echo 8 > functions/hid.usb0/report_length
cat hid_report_main.bin > functions/hid.usb0/report_desc   # 기존 매크로를 2진으로 추출
# HID 함수 2: 상대마우스 + digitizer (리포트 ID 5,7)
mkdir -p functions/hid.usb1
echo 0 > functions/hid.usb1/protocol
echo 0 > functions/hid.usb1/subclass
echo 8 > functions/hid.usb1/report_length
cat hid_report_rel.bin > functions/hid.usb1/report_desc
ln -s functions/hid.usb0 configs/c.1/
ln -s functions/hid.usb1 configs/c.1/
echo dwc2-otg-0 > UDC          # PC B 쪽은 별도 gadget 디렉터리 + 다른 UDC
```

포인트:
- **configfs는 UDC 하나당 gadget 하나**만 bind 가능 → gadget 디렉터리를 2벌 만든다.
- 인터페이스(=HID 함수)는 **엔드포인트 예산**에 맞춰 합쳐야 한다. DeskHop은 PC당
  `ITF_NUM_HID`(1~4) / `ITF_NUM_HID_REL_M`(5,7) / `ITF_NUM_HID_VENDOR`(6) 3개 구조인데,
  벤더 IF는 불필요하므로 2개로 줄인다. dwc2 계열의 IN EP 수(보통 4~6) 안에 들어간다.

### 3.2 데몬

- 단일 C/Rust 프로세스, `epoll`로 hidraw fd + hidg fd 감시
- `EVIOCGRAB`(또는 hidraw 단독 open)으로 SBC 자신의 OS가 키 입력을 먹지 않게 격리
- 키보드는 hidraw raw report를 그대로 전달. 단 **인터페이스→리포트 매핑**은 남는다:
  `HIDIOCGRDESC`로 리포트 디스크립터를 읽어 "이 노드가 키보드인지 컨슈머인지" 판별
  (기존 `hid_parser.c`가 하던 일을 커널이 디스크립터 제공으로 대신해주는 형태)
- LED: PC가 보내는 SET_REPORT는 `hidg`의 `read()`로 들어온다 → 키보드 쪽 hidraw write
  또는 `/sys/class/leds/inputX::capslock`으로 전달 (DeskHop의 `keyboard_leds_*` 로직 그대로)
- 실시간성: `SCHED_FIFO` 80+, `isolcpus`+`irqaffinity`로 격리, hidraw는 non-blocking 루프,
  USB IRQ는 해당 코어에 고정

---

## 4. SBC로 갈 때의 실제 난점

| # | 항목 | 내용 | 대응 |
|---|---|---|---|
| 1 | **UDC 2개** | 대부분의 SBC는 dwc2/dwc3 **1개**. 멀티 OTG SoC(RK3399/RK3588, i.MX8M, AM62x, STM32MP1 계열)에서만 2개. 보드 스펙의 "USB-C 2개"가 전부 device 가능하다는 뜻은 아님 | `ls /sys/class/udc`, dts에서 `dr_mode = "otg"/"peripheral"` 확인 |
| 2 | **엔드포인트 예산** | gadget당 HID 함수마다 IN 인터럽트 EP 1개. 인터페이스 재편 필요 | 2~3 인터페이스로 통합 |
| 3 | **지연/지터** | RP2040: 전용 루프 `_TOP()`, 폴링 1000Hz, 스위치 즉시. SBC: hidraw→userspace→`write(/dev/hidgN)`→커널이 다음 IN 토큰에 전송. 실측 기대 2~8ms(지터 포함), RT 튜닝 시 1~3ms | 타이핑/KVM은 충분, **8K 게이밍 마우스에는 부족** |
| 4 | **절연 소실** | DeskHop의 2kV 절연이 사라짐. 두 PC + SBC의 그라운드가 한 점에서 만남 → 그라운드 루프, 누설전류 경로 | PC당 `ISOUSB211`(HS 480Mbps) 1개씩, VBUS는 절연측에서 공급. FS면 ADuM3160 |
| 5 | **전원/시퀀스** | DeskHop은 PC가 전원을 줌(꺼져도 대부분 공급). SBC는 자체 PSU 필요, 부팅 15~30초 동안 두 PC는 HID가 붙지 않음 | 상시 전원, 부팅 후 gadget bind |
| 6 | **단일 장애점** | 커널 패닉/업데이트 실패 = **양쪽 PC 입력 동시 상실**. 복구용 키보드를 따로 꽂아야 함 | watchdog, A/B rootfs, 물리 bypass 스위치 |
| 7 | **보안 모델** | DeskHop은 "네트워크 없음, PC 간 정보 경로 없음, 아웃바운드에서 FW 조작 불가"를 설계 원칙으로 명시. 리눅스에서는 그 보장이 원리적으로 사라짐 (이 포크는 이미 CDC 터널로 경로를 열었으므로 어느 정도 수용한 셈) | 정책적으로 수용할지 결정 |
| 8 | Windows/멀티모니터 제약 | 리포트 디스크립터가 같으므로 SBC로 가도 동일하게 존재 | 변화 없음 (오히려 디스크립터 재사용이 장점) |

---

## 5. 대안 비교 — "SBC 1대"가 최선인가?

| 구성 | 지연 | 절연 | 단일보드 | 코드 재작성 | 비용 | 총평 |
|---|---|---|---|---|---|---|
| **2× Pico (현행)** | 최상 (µs급) | O | ✗ | – | ~12€ | 지금이 이미 최적해에 가깝다 |
| **1× 듀얼 OTG MCU** (STM32F407/H723, i.MX RT1062, SAM E70 등) | 최상 | 별도 부품 | **O** | 중 (TinyUSB 계층 유지, 링크 계층만 제거) | 10~20€ | **"보드 1개"가 목표라면 가장 실용적** |
| **1× SBC** | 보통 (1~8ms) | 별도 + 비쌈 | O | 대 (USB 계층 전면 재작성) | 40~150€ | 유연성/설정편의 최고, 실시간성·신뢰성 열위 |
| SBC + Pico 2장 | 좋음 | O | ✗ | 중 | 높음 | 보드 수 이점 없음 |

**핵심 판단**: "2보드가 싫다"가 동기라면 듀얼 OTG MCU가 정답에 가깝다. 같은 TinyUSB를 쓰므로
`usb.c`/`hid_parser.c`/`handlers.c`/`mouse.c`가 거의 그대로 살고, `uart.c`/`protocol.c`만
함수 호출로 바뀐다(실질 1~2주 규모의 포팅). 반면 SBC는 "리눅스 앱 개발"이 되어 2~4주+이고,
얻는 것은 HTTP 설정·로그·OTA·터널/미러의 손쉬움, 잃는 것은 실시간성·절연·무상태 신뢰성이다.

---

## 6. SBC로 간다면: 작업 순서 (현실적 순서)

1. **하드웨어 검증 먼저** (코드보다 앞)
   `ls /sys/class/udc`가 2개인지, 각 UDC가 FS/HS 어느 쪽인지, `configfs` 마운트 여부,
   EP 수(`dmesg | grep -i dwc`). 이게 안 되면 설계 자체가 무효.
2. `usb_descriptors.h` 매크로에서 **리포트 디스크립터 2진 blob 2개** 추출 (전처리 + objcopy).
3. configfs 셋업 스크립트 작성 → 두 PC에 각각 "DeskHop" HID 복합장치로 인식되는지 확인.
   이 단계에서 마우스가 PC에서 실제로 움직이는지가 첫 마일스톤.
4. `hidraw` 리더 + **raw passthrough** 데몬 (핫키/스위치 없이 일단 한 PC로만).
5. `mouse.c` 로직 이식: 절대좌표 누적, 가속 곡선, `jump_threshold`, 보더 점프.
6. `keyboard.c` 핫키 표 + `handlers.c` 액션 이식 (스위치 락/게이밍/미러/스크린세이버).
7. LED 피드백(SET_REPORT → hidraw/leds), 설정 UI는 `webconfig/`를 HTTP로 서빙.
8. **지연 측정**: PC A에서 hidraw로 마이크로초 카운터를 주입 → PC B에서 수신 시각과 비교
   (또는 USB 로직 분석기). 목표: p99 < 4ms.
9. 부팅 자동화 + watchdog + (필요시) bypass.

---

## 7. 최종 판단

- **"가능한가?"** → 예. OTG 2개(=UDC 2개)가 실제로 있는 보드라면 두 PC에 HID 복합장치로
  붙는 것까지는 커널 기능만으로 문제없이 된다. 리포트 디스크립터도 기존 것을 그대로 쓰므로
  Windows/macOS 호환성 문제도 새로 생기지 않는다.
- **"그게 DeskHop을 '대체'하는가?"** → 아니오, **다른 제품을 새로 만드는 것**에 가깝다.
  7.9k줄 중 재사용 가능한 것은 로직 1.3~1.6k줄(mouse/keyboard/handlers/설정)과 디스크립터 표뿐이고,
  하드웨어 계층·링크 계층·플래시/설정드라이브(약 4k줄)는 통째로 사라진다.
- **얻는 것**: 보드 1장, 진짜 웹 설정, 로그/디버깅, OTA, 터널·미러 같은 기능이 공짜,
  3개 이상 출력으로 확장도 상대적으로 쉬움(UDC가 더 있으면).
- **잃는 것**: µs급 지연(→ 1~8ms), 갈바닉 절연, 무상태 신뢰성, "네트워크 없는 입력장치"라는 보안 성질.
- **권고**: 목표가 "보드 개수 줄이기"라면 **듀얼 OTG MCU 1장**. 목표가 "설정/확장성/부가기능"이라면
  **SBC 1대**(단, 지연과 절연은 감수). 둘 다면 SBC + `ISOUSB211` 2개 구성이 현실적 타협점.
