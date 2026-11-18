package writer

// Host-side client for the "DW" protocol: it drives the board's USB flash
// drive through the CDC port.
//
// Nothing in here is Windows specific (the serial port arrives as a Port
// interface), so the whole state machine is testable on any platform against a
// fake board -- see client_test.go.

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"time"
)

// Port is a non-blocking byte pipe to the board (the CDC port).
//
// Read must return (0, nil) when no bytes are currently available instead of
// blocking, so the client can enforce its own timeouts and stay cancellable.
type Port interface {
	Write(p []byte) error
	Read(p []byte) (int, error)
}

// Errors returned by the client.
var (
	// ErrTimeout means the board did not answer in time. The usual causes are
	// a wrong COM port, the board sitting in mirror mode, or a firmware
	// without the Writer variant flashed.
	ErrTimeout = errors.New("보드 응답 타임아웃")
	// ErrCancelled is returned when the caller closed the cancel channel.
	ErrCancelled = errors.New("사용자가 취소했습니다")
	// ErrTooLarge means the file does not fit in the protocol's 32-bit offset
	// space (4 GiB - 1).
	ErrTooLarge = errors.New("파일이 4GiB 를 넘습니다 (프로토콜 제한)")
)

// Default timeouts. Control frames are answered by a board that only has to
// touch FatFs; DATA_ACKs arrive after real flash writes, so they get more room.
const (
	defaultCtlTimeout  = 2 * time.Second
	defaultDataTimeout = 15 * time.Second
	defaultAttempts    = 4
	pollInterval       = 2 * time.Millisecond
)

// HelloInfo is the board's answer to HELLO.
type HelloInfo struct {
	Version uint8
	MaxData int    // largest DATA chunk the board accepts
	Caps    uint8  // CapExfat | CapResume
	Status  Status // medium state at handshake time
}

// Exfat reports whether the firmware was built with exFAT support.
func (h HelloInfo) Exfat() bool { return h.Caps&CapExfat != 0 }

// ResumeSupported reports whether OPEN may ask for resume.
func (h HelloInfo) ResumeSupported() bool { return h.Caps&CapResume != 0 }

// DiskInfo describes the flash drive currently attached to the board.
type DiskInfo struct {
	Status   Status
	Mounted  bool
	FSType   uint8
	Writable bool
	TotalKB  uint32
	FreeKB   uint32
	Label    string
}

// FSTypeName maps the firmware's fs_type byte to a readable name. The byte is
// FatFs's FS_* enum, so FAT32 is 3 and exFAT is 4 -- do not shift these.
func (d DiskInfo) FSTypeName() string {
	switch d.FSType {
	case FSTypeNone:
		return "없음"
	case FSTypeFAT12:
		return "FAT12"
	case FSTypeFAT16:
		return "FAT16"
	case FSTypeFAT32:
		return "FAT32"
	case FSTypeExfat:
		return "exFAT"
	default:
		return fmt.Sprintf("알 수 없음(%d)", d.FSType)
	}
}

// String renders the drive summary for logs.
func (d DiskInfo) String() string {
	if !d.Mounted {
		return "USB 메모리 없음"
	}
	ro := ""
	if !d.Writable {
		ro = ", 쓰기 금지"
	}
	label := d.Label
	if label == "" {
		label = "(라벨 없음)"
	}
	return fmt.Sprintf("%s (%s), %d KB 중 %d KB 여유%s",
		label, d.FSTypeName(), d.TotalKB, d.FreeKB, ro)
}

// OpenResult is the board's answer to OPEN.
type OpenResult struct {
	Status Status
	Offset uint32 // byte offset the session starts writing at
}

// FileResult summarises one finished file.
type FileResult struct {
	Name        string
	Size        uint32
	ResumedFrom uint32 // > 0 when a partial file was continued
	Sent        uint32 // bytes pushed in this session
	CRC         uint32 // CRC-32 of the bytes written in this session
}

// Options tunes PutFile.
type Options struct {
	// Resume asks the board to continue an existing partial file when its
	// prefix matches (the board only knows the size, so the caller should use
	// this only when re-sending the same file).
	Resume bool
	// Chunk is the DATA payload size, 512..ChunkSize. 0 picks ChunkSize.
	Chunk int
	// Cancel may be closed by the caller to abort the transfer. The client
	// then sends ABORT so the board closes the file handle.
	Cancel <-chan struct{}
	// Progress receives the running byte counters (may be nil).
	Progress func(Progress)
	// Attempts is how often a control frame is retried on timeout.
	Attempts int
}

// Progress is a snapshot handed to Options.Progress.
type Progress struct {
	Name      string        // destination path on the stick
	Sent      int64         // bytes written in this session
	Total     int64         // file size
	Resumed   uint32        // nonzero when continuing a partial file
	Index     int           // file number in the batch (1-based, 0 = single)
	Batch     int           // files in the batch
	Elapsed   time.Duration // time spent on this file
	Complete  bool
	RateBytes float64
}

// Percent returns 0..100.
func (p Progress) Percent() int {
	if p.Total <= 0 {
		if p.Complete {
			return 100
		}
		return 0
	}
	if p.Sent >= p.Total {
		return 100
	}
	n := int(p.Sent * 100 / p.Total)
	if n > 99 {
		n = 99
	}
	return n
}

// Client speaks the DW protocol over one Port.
type Client struct {
	port Port
	dec  Decoder
	buf  []byte

	// Logf receives readable trace lines when set.
	Logf func(string)

	CtlTimeout  time.Duration
	DataTimeout time.Duration
}

// New returns a client reading and writing through p.
func New(p Port) *Client {
	return &Client{
		port:        p,
		buf:         make([]byte, 4096),
		CtlTimeout:  defaultCtlTimeout,
		DataTimeout: defaultDataTimeout,
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(fmt.Sprintf(format, args...))
	}
}

// Reset drops buffered input (call after opening the port or after a timeout).
func (c *Client) Reset() { c.dec.Reset() }

// BadBytes reports how many bytes were discarded while resynchronising.
func (c *Client) BadBytes() int { return c.dec.Bad }

// ---- framing -------------------------------------------------------------

// pump reads whatever the port has and feeds the decoder.
func (c *Client) pump() error {
	for {
		n, err := c.port.Read(c.buf)
		if err != nil {
			return err
		}
		if n <= 0 {
			return nil
		}
		c.dec.Feed(c.buf[:n])
	}
}

// next waits for any frame.
func (c *Client) next(timeout time.Duration) (Frame, error) {
	deadline := time.Now().Add(timeout)
	for {
		if f, ok := c.dec.Next(); ok {
			return f, nil
		}
		if err := c.pump(); err != nil {
			return Frame{}, err
		}
		if f, ok := c.dec.Next(); ok {
			return f, nil
		}
		if time.Now().After(deadline) {
			return Frame{}, ErrTimeout
		}
		time.Sleep(pollInterval)
	}
}

// expect waits for one of want and turns ERR replies into StatusError.
// Frames of other types (a late DATA_ACK, for instance) are skipped.
func (c *Client) expect(op string, timeout time.Duration, want ...Type) (Frame, error) {
	deadline := time.Now().Add(timeout)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return Frame{}, ErrTimeout
		}
		f, err := c.next(remain)
		if err != nil {
			return Frame{}, err
		}
		if f.Type == TErr {
			st := StFrameErr
			if len(f.Payload) > 0 {
				st = Status(f.Payload[0])
			}
			c.logf("%s: 보드가 ERR 0x%02X 를 보냈습니다", op, uint8(st))
			return Frame{}, &StatusError{Op: op, Code: st}
		}
		for _, w := range want {
			if f.Type == w {
				return f, nil
			}
		}
		c.logf("%s: 예상치 못한 %s 프레임 무시", op, f.Type)
	}
}

// request sends one frame and waits for its reply, retrying on timeout.
func (c *Client) request(op string, typ Type, payload []byte, ack Type, attempts int) (Frame, error) {
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := c.port.Write(Encode(typ, payload)); err != nil {
			return Frame{}, err
		}
		f, err := c.expect(op, c.CtlTimeout, ack)
		if err == nil {
			return f, nil
		}
		if errors.Is(err, ErrTimeout) {
			lastErr = err
			c.logf("%s: 응답 없음 (%d/%d) 재시도", op, i+1, attempts)
			continue
		}
		return Frame{}, err
	}
	return Frame{}, lastErr
}

// ---- commands ------------------------------------------------------------

// Hello performs the handshake and returns the board's capabilities.
func (c *Client) Hello() (HelloInfo, error) {
	f, err := c.request("HELLO", THello, []byte{Version}, THelloAck, defaultAttempts)
	if err != nil {
		return HelloInfo{}, err
	}
	if len(f.Payload) < 5 {
		return HelloInfo{}, fmt.Errorf("HELLO_ACK 길이 이상: %d", len(f.Payload))
	}
	p := f.Payload
	info := HelloInfo{
		Version: p[0],
		MaxData: int(uint16(p[1]) | uint16(p[2])<<8),
		Caps:    p[3],
		Status:  Status(p[4]),
	}
	c.logf("HELLO_ACK: ver=%d maxdata=%d caps=0x%02X status=%s",
		info.Version, info.MaxData, info.Caps, info.Status)
	if info.Version != Version {
		return info, fmt.Errorf("프로토콜 버전 불일치: 보드 %d, 프로그램 %d", info.Version, Version)
	}
	return info, nil
}

// Stat reports the attached drive.
func (c *Client) Stat() (DiskInfo, error) {
	f, err := c.request("STAT", TStat, nil, TStatAck, defaultAttempts)
	if err != nil {
		return DiskInfo{}, err
	}
	if len(f.Payload) < 12 {
		return DiskInfo{}, fmt.Errorf("STAT_ACK 길이 이상: %d", len(f.Payload))
	}
	p := f.Payload
	rd32 := func(b []byte) uint32 {
		return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	}
	info := DiskInfo{
		Status:   Status(p[0]),
		Mounted:  p[1] != 0,
		FSType:   p[2],
		Writable: p[3] != 0,
		TotalKB:  rd32(p[4:]),
		FreeKB:   rd32(p[8:]),
	}
	if len(p) > 12 {
		end := 12 + 11
		if end > len(p) {
			end = len(p)
		}
		info.Label = strings.TrimRight(string(p[12:end]), "\x00")
	}
	return info, nil
}

// Open starts a file. path is relative to the drive root, '/'-separated and
// UTF-8 encoded; missing parent directories are created by the board.
func (c *Client) Open(path string, size uint32, resume bool) (OpenResult, error) {
	clean, err := cleanPath(path)
	if err != nil {
		return OpenResult{}, err
	}
	if len(clean) > MaxPathLen {
		return OpenResult{}, fmt.Errorf("경로가 너무 깁니다 (%d > %d 바이트)", len(clean), MaxPathLen)
	}

	payload := make([]byte, 0, 5+len(clean)+1)
	var flags byte
	if resume {
		flags |= OpenResume
	}
	payload = append(payload, flags)
	var sz [4]byte
	sz[0] = byte(size)
	sz[1] = byte(size >> 8)
	sz[2] = byte(size >> 16)
	sz[3] = byte(size >> 24)
	payload = append(payload, sz[:]...)
	payload = append(payload, clean...)
	payload = append(payload, 0)

	f, err := c.request("OPEN", TOpen, payload, TOpenAck, defaultAttempts)
	if err != nil {
		return OpenResult{}, err
	}
	if len(f.Payload) < 5 {
		return OpenResult{}, fmt.Errorf("OPEN_ACK 길이 이상: %d", len(f.Payload))
	}
	res := OpenResult{
		Status: Status(f.Payload[0]),
		Offset: uint32(f.Payload[1]) | uint32(f.Payload[2])<<8 |
			uint32(f.Payload[3])<<16 | uint32(f.Payload[4])<<24,
	}
	if res.Status != StOK {
		return res, &StatusError{Op: "OPEN " + clean, Code: res.Status}
	}
	return res, nil
}

// data sends one DATA frame (offset must match the board's current position).
func (c *Client) data(off uint32, chunk []byte) error {
	frame := make([]byte, 4+len(chunk))
	frame[0] = byte(off)
	frame[1] = byte(off >> 8)
	frame[2] = byte(off >> 16)
	frame[3] = byte(off >> 24)
	copy(frame[4:], chunk)
	return c.port.Write(Encode(TData, frame))
}

// Close finishes the file and asks the board to verify size and CRC.
// crc must cover exactly the bytes written in this session (from the OPEN
// resume offset onwards).
func (c *Client) Close(size, crc uint32) (Status, error) {
	payload := make([]byte, 8)
	putU32(payload[0:], size)
	putU32(payload[4:], crc)

	f, err := c.request("CLOSE", TClose, payload, TCloseAck, defaultAttempts)
	if err != nil {
		return 0, err
	}
	if len(f.Payload) < 1 {
		return 0, fmt.Errorf("CLOSE_ACK 길이 이상: %d", len(f.Payload))
	}
	return Status(f.Payload[0]), nil
}

// Abort drops the open file handle, keeping whatever already landed.
func (c *Client) Abort() error {
	f, err := c.request("ABORT", TAbort, nil, TAbortAck, 2)
	if err != nil {
		return err
	}
	if len(f.Payload) > 0 && Status(f.Payload[0]) != StOK {
		return &StatusError{Op: "ABORT", Code: Status(f.Payload[0])}
	}
	return nil
}

// Eject flushes and releases the drive so it can be unplugged safely.
func (c *Client) Eject() error {
	f, err := c.request("EJECT", TEject, nil, TEjectAck, defaultAttempts)
	if err != nil {
		return err
	}
	if len(f.Payload) > 0 {
		if st := Status(f.Payload[0]); st != StOK {
			return &StatusError{Op: "EJECT", Code: st}
		}
	}
	return nil
}

// ---- file transfer -------------------------------------------------------

// PutFile copies localPath to path on the flash drive.
func (c *Client) PutFile(localPath, path string, opts Options, index, batch int) (FileResult, error) {
	chunk := opts.Chunk
	if chunk <= 0 || chunk > ChunkSize {
		chunk = ChunkSize
	}
	if chunk < 512 {
		chunk = 512
	}

	var res FileResult
	res.Name = path

	st, err := os.Stat(localPath)
	if err != nil {
		return res, err
	}
	if st.IsDir() {
		return res, fmt.Errorf("%s 는 폴더입니다", localPath)
	}
	if st.Size() > int64(^uint32(0)) {
		return res, ErrTooLarge
	}
	res.Size = uint32(st.Size())
	total := st.Size()

	start := time.Now()
	lastProg := time.Time{}
	emit := func(sent int64, complete bool) {
		if opts.Progress == nil {
			return
		}
		if !complete && time.Since(lastProg) < 100*time.Millisecond {
			return
		}
		lastProg = time.Now()
		el := time.Since(start)
		rate := 0.0
		if secs := el.Seconds(); secs > 0 {
			rate = float64(sent) / secs
		}
		opts.Progress(Progress{
			Name:      res.Name,
			Sent:      sent,
			Total:     total,
			Resumed:   res.ResumedFrom,
			Index:     index,
			Batch:     batch,
			Elapsed:   el,
			Complete:  complete,
			RateBytes: rate,
		})
	}

	fh, err := os.Open(localPath)
	if err != nil {
		return res, err
	}
	defer fh.Close()

	open, err := c.Open(path, res.Size, opts.Resume)
	// A refused OPEN still leaves a session open on the board (it reports the
	// status but keeps going), so make sure nothing is left half-open.
	if err != nil {
		return res, err
	}
	if open.Offset > res.Size {
		_ = c.Abort()
		return res, fmt.Errorf("보드가 보고한 이어받기 위치(%d)가 파일 크기(%d)보다 큽니다",
			open.Offset, res.Size)
	}
	res.ResumedFrom = open.Offset
	if open.Offset > 0 {
		if _, err := fh.Seek(int64(open.Offset), io.SeekStart); err != nil {
			_ = c.Abort()
			return res, err
		}
		c.logf("%s: %d 바이트부터 이어받기", path, open.Offset)
	}

	sum := crc32.NewIEEE()
	off := open.Offset
	buf := make([]byte, chunk)
	frames := 0

	for {
		if isCancelled(opts.Cancel) {
			_ = c.Abort()
			return res, ErrCancelled
		}
		n, rerr := fh.Read(buf)
		if n > 0 {
			if err := c.data(off, buf[:n]); err != nil {
				_ = c.Abort()
				return res, err
			}
			sum.Write(buf[:n])
			off += uint32(n)
			res.Sent += uint32(n)
			frames++
			emit(int64(off), false)

			if frames >= AckEvery {
				frames = 0
				if err := c.awaitDataAck("DATA", off); err != nil {
					_ = c.Abort()
					return res, err
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = c.Abort()
			return res, rerr
		}
	}

	res.CRC = sum.Sum32()

	st2, err := c.Close(res.Size, res.CRC)
	if err != nil {
		return res, err
	}
	if st2 != StOK {
		return res, &StatusError{Op: "CLOSE " + path, Code: st2}
	}
	emit(int64(res.Size), true)
	return res, nil
}

// awaitDataAck waits for the board's periodic progress report and checks that
// both sides agree on the write position.
func (c *Client) awaitDataAck(op string, expected uint32) error {
	f, err := c.expect(op, c.DataTimeout, TDataAck)
	if err != nil {
		return err
	}
	if len(f.Payload) < 5 {
		return fmt.Errorf("DATA_ACK 길이 이상: %d", len(f.Payload))
	}
	st := Status(f.Payload[0])
	written := uint32(f.Payload[1]) | uint32(f.Payload[2])<<8 |
		uint32(f.Payload[3])<<16 | uint32(f.Payload[4])<<24
	if st != StOK {
		return &StatusError{Op: fmt.Sprintf("DATA (%d 바이트 지점)", written), Code: st}
	}
	if written != expected {
		return fmt.Errorf("보드 기록 위치 불일치: 보드 %d, 예상 %d", written, expected)
	}
	return nil
}

// ---- helpers -------------------------------------------------------------

func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

// cleanPath validates a destination path: relative, '/'-separated, no "..",
// no drive letter, no leading slash. The board repeats these checks, this just
// fails early with a readable message.
func cleanPath(p string) (string, error) {
	s := strings.ReplaceAll(p, "\\", "/")
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "/") {
		s = s[1:]
	}
	if s == "" {
		return "", errors.New("저장 경로가 비어 있습니다")
	}
	if strings.ContainsRune(s, ':') {
		return "", fmt.Errorf("경로에 ':' 는 쓸 수 없습니다: %s", p)
	}
	for _, part := range strings.Split(s, "/") {
		switch part {
		case "":
			return "", fmt.Errorf("경로에 빈 항목이 있습니다: %s", p)
		case ".", "..":
			return "", fmt.Errorf("경로에 '%s' 는 쓸 수 없습니다: %s", part, p)
		}
	}
	return s, nil
}

func isCancelled(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
