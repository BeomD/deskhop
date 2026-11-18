// Package writer implements the "DW" wire protocol that the DeskHop Writer
// firmware variant speaks over its CDC port.
//
// The board acts as a USB mass-storage *host*: a USB flash drive is plugged
// into the board itself and the host PC only sends file contents, never sees
// the stick. The protocol therefore looks like a tiny remote filesystem API.
//
//	'D' 'W' | type(1) | len(2 LE) | payload[len] | crc32(4 LE)
//
// crc32 is CRC-32/IEEE (zlib, as in Go's hash/crc32) over type..payload, i.e.
// the 3+len bytes that follow the two magic bytes. The total frame length is
// always 9+len.
//
// Payload limits are asymmetric on purpose: control frames (paths) are capped
// at CtlMax, DATA frames carry a 4-byte offset plus up to ChunkSize bytes.
package writer

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// Magic bytes. Anything that does not start with "DW" on the CDC port is
// relayed by the board to the peer board (the normal DeskHop tunnel), which is
// how writer sessions coexist with file transfer between two PCs.
const (
	Magic0 = 'D'
	Magic1 = 'W'

	// Version is the protocol version reported by HELLO_ACK.
	Version = 1

	// CtlMax is the largest payload of a non-DATA frame.
	CtlMax = 208
	// DataMax is the largest DATA payload (4 offset bytes + chunk).
	DataMax = 4100
	// ChunkSize is the largest chunk one DATA frame may carry.
	ChunkSize = DataMax - 4
	// MaxPathLen is the longest path an OPEN frame may carry, excluding the
	// terminating NUL. The frame itself would allow CtlMax-5-1 = 202 bytes,
	// but the firmware's validator (DH_DISK_MAX_PATH in usb_disk.h) rejects
	// anything longer than 200, so clamp to the stricter of the two and fail
	// here with a readable message instead of an OPEN DH_DISK_BAD_PATH.
	MaxPathLen = 200
	// AckEvery is how many DATA frames the board accepts before it reports
	// progress with a DATA_ACK (or immediately, on error).
	AckEvery = 8
	// FrameOverhead is len("DW") + type + len + crc32.
	FrameOverhead = 9
)

// Type is a frame type byte.
type Type uint8

// Host -> board frames.
const (
	THello Type = 0x01
	TStat  Type = 0x02
	TOpen  Type = 0x04
	TData  Type = 0x05
	TClose Type = 0x06
	TAbort Type = 0x07
	TEject Type = 0x08
)

// Board -> host frames.
const (
	THelloAck Type = 0x81
	TStatAck  Type = 0x82
	TOpenAck  Type = 0x84
	TDataAck  Type = 0x85
	TCloseAck Type = 0x86
	TAbortAck Type = 0x87
	TEjectAck Type = 0x88
	TErr      Type = 0x8F
)

// String renders a frame type for logs.
func (t Type) String() string {
	switch t {
	case THello:
		return "HELLO"
	case TStat:
		return "STAT"
	case TOpen:
		return "OPEN"
	case TData:
		return "DATA"
	case TClose:
		return "CLOSE"
	case TAbort:
		return "ABORT"
	case TEject:
		return "EJECT"
	case THelloAck:
		return "HELLO_ACK"
	case TStatAck:
		return "STAT_ACK"
	case TOpenAck:
		return "OPEN_ACK"
	case TDataAck:
		return "DATA_ACK"
	case TCloseAck:
		return "CLOSE_ACK"
	case TAbortAck:
		return "ABORT_ACK"
	case TEjectAck:
		return "EJECT_ACK"
	case TErr:
		return "ERR"
	default:
		return fmt.Sprintf("0x%02X", uint8(t))
	}
}

// OPEN flags.
const (
	OpenResume = 0x01
)

// HELLO_ACK capability bits.
const (
	CapExfat  = 0x01
	CapResume = 0x02
)

// fs_type values carried by STAT_ACK. The firmware sends FatFs's FS_* enum
// verbatim (see lib/fatfs/ff.h: FS_FAT12 = 1, FS_FAT16 = 2, FS_FAT32 = 3,
// FS_EXFAT = 4), so keep these in sync with that enum rather than inventing a
// separate numbering. 0 means "no volume mounted".
const (
	FSTypeNone  = 0
	FSTypeFAT12 = 1
	FSTypeFAT16 = 2
	FSTypeFAT32 = 3
	FSTypeExfat = 4
)

// Status is the medium/operation result code shared with the firmware's
// dh_disk_status_t.
type Status uint8

// Status codes. FrameErr is not a medium error: it means the frame itself was
// malformed (bad length or CRC).
const (
	StOK          Status = 0
	StIO          Status = 1
	StNoMedia     Status = 2
	StNoSpace     Status = 3
	StBadPath     Status = 4
	StReadOnly    Status = 5
	StBusy        Status = 6
	StCRCMismatch Status = 7
	StNotOpen     Status = 8
	StBadArg      Status = 9
	StUnsupported Status = 10
	StFrameErr    Status = 0xFF
)

// String renders a status in Korean, matching the firmware meanings.
func (s Status) String() string {
	switch s {
	case StOK:
		return "성공"
	case StIO:
		return "디스크 입출력 오류"
	case StNoMedia:
		return "USB 메모리가 없습니다"
	case StNoSpace:
		return "USB 메모리 공간 부족"
	case StReadOnly:
		return "쓰기 금지된 볼륨"
	case StBadPath:
		return "잘못된 경로"
	case StBusy:
		return "디스크가 사용 중입니다"
	case StCRCMismatch:
		return "CRC 불일치 (파일 손상)"
	case StNotOpen:
		return "파일이 열려 있지 않습니다"
	case StBadArg:
		return "잘못된 인자/크기 불일치"
	case StUnsupported:
		return "보드가 지원하지 않는 요청"
	case StFrameErr:
		return "프레임 오류 (길이/CRC)"
	default:
		return fmt.Sprintf("알 수 없는 상태(%d)", uint8(s))
	}
}

// StatusError wraps a non-zero Status returned by the board.
type StatusError struct {
	Op   string
	Code Status
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s 실패: %s (0x%02X)", e.Op, e.Code, uint8(e.Code))
}

// Encode builds a complete frame: magic, type, length, payload, CRC-32.
func Encode(t Type, payload []byte) []byte {
	f := make([]byte, FrameOverhead+len(payload))
	f[0] = Magic0
	f[1] = Magic1
	f[2] = byte(t)
	binary.LittleEndian.PutUint16(f[3:5], uint16(len(payload)))
	copy(f[5:], payload)
	crc := crc32.ChecksumIEEE(f[2 : 5+len(payload)])
	binary.LittleEndian.PutUint32(f[5+len(payload):], crc)
	return f
}

// Frame is one decoded protocol frame.
type Frame struct {
	Type    Type
	Payload []byte
}

// Decoder turns a byte stream into frames. It is deliberately forgiving: it
// resynchronises on the next "DW" pair after line noise and drops frames whose
// CRC or length does not check out, exactly like the firmware parser does.
type Decoder struct {
	buf []byte
	// Bad counts bytes that were discarded while resynchronising. Useful in
	// diagnostics; a healthy link keeps this at zero.
	Bad int
}

// Feed appends received bytes to the decoder.
func (d *Decoder) Feed(p []byte) {
	d.buf = append(d.buf, p...)
}

// Pending reports how many bytes are buffered but not yet a full frame.
func (d *Decoder) Pending() int { return len(d.buf) }

// Next returns the next complete frame. ok is false when more bytes are
// needed.
func (d *Decoder) Next() (f Frame, ok bool) {
	for {
		// Skip anything that cannot be the start of a frame.
		i := 0
		for i+1 < len(d.buf) && !(d.buf[i] == Magic0 && d.buf[i+1] == Magic1) {
			i++
		}
		if i > 0 {
			d.Bad += i
			d.buf = d.buf[i:]
		}
		if len(d.buf) < FrameOverhead-4 { // magic + type + len
			return Frame{}, false
		}

		typ := Type(d.buf[2])
		ln := int(binary.LittleEndian.Uint16(d.buf[3:5]))

		limit := CtlMax
		if typ == TData {
			limit = DataMax
		}
		if ln > limit {
			// A "DW" that is followed by an impossible length is line noise,
			// not a frame: rescan from the next byte so one bad guess does not
			// swallow the frames behind it.
			d.Bad++
			d.buf = d.buf[1:]
			continue
		}
		if len(d.buf) < FrameOverhead+ln {
			return Frame{}, false
		}

		want := binary.LittleEndian.Uint32(d.buf[5+ln : 9+ln])
		if got := crc32.ChecksumIEEE(d.buf[2 : 5+ln]); got != want {
			d.Bad++
			d.buf = d.buf[1:]
			continue
		}

		payload := make([]byte, ln)
		copy(payload, d.buf[5:5+ln])
		d.buf = d.buf[FrameOverhead+ln:]
		return Frame{Type: typ, Payload: payload}, true
	}
}

// Reset drops any buffered bytes (call after re-opening the port).
func (d *Decoder) Reset() {
	d.buf = d.buf[:0]
}
