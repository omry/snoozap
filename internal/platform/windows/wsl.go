//go:build windows

package windows

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/omry/snoozap/internal/model"
	"github.com/omry/snoozap/internal/probe/t3"
)

type WSL struct {
	Distribution string
	Run          func(context.Context, []string, []byte) ([]byte, error)
}

func systemDirectory() (string, error) {
	data := make([]uint16, 32768)
	n, _, _ := kernel.NewProc("GetSystemDirectoryW").Call(uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))
	if n == 0 || n >= uintptr(len(data)) {
		return "", t3.ErrUnavailable
	}
	return syscall.UTF16ToString(data[:n]), nil
}

type commandOutput struct {
	buffer bytes.Buffer
	limit  int
	cancel context.CancelFunc
}

func (b *commandOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.cancel()
		return 0, t3.ErrIncomplete
	}
	return b.buffer.Write(p)
}

func (w WSL) run(ctx context.Context, args []string, input []byte) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if w.Run != nil {
		return w.Run(ctx, args, input)
	}
	dir, err := systemDirectory()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(dir, "wsl.exe"), args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	out := &commandOutput{limit: t3.MaxBytes + (16 << 10), cancel: cancel}
	cmd.Stdout = out
	if cmd.Run() != nil {
		return nil, t3.ErrUnavailable
	}
	return out.buffer.Bytes(), nil
}

func distributionLines(data []byte) ([]string, error) {
	if bytes.IndexByte(data, 0) >= 0 || bytes.HasPrefix(data, []byte{0xff, 0xfe}) {
		if len(data)%2 != 0 {
			return nil, t3.ErrIncomplete
		}
		words := make([]uint16, len(data)/2)
		for i := range words {
			words[i] = binary.LittleEndian.Uint16(data[i*2:])
		}
		if len(words) > 0 && words[0] == 0xfeff {
			words = words[1:]
		}
		for i := 0; i < len(words); i++ {
			if words[i] >= 0xd800 && words[i] <= 0xdbff {
				if i+1 == len(words) || words[i+1] < 0xdc00 || words[i+1] > 0xdfff {
					return nil, t3.ErrSchema
				}
				i++
			} else if words[i] >= 0xdc00 && words[i] <= 0xdfff {
				return nil, t3.ErrSchema
			}
		}
		data = []byte(string(utf16.Decode(words)))
	}
	if len(data) > t3.MaxBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, t3.ErrSchema
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		return nil, t3.ErrIncomplete
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n"), nil
}

func (w WSL) running(ctx context.Context) error {
	if !t3.Distribution(w.Distribution) {
		return t3.ErrIdentity
	}
	data, err := w.run(ctx, []string{"--list", "--running", "--quiet"}, nil)
	if err != nil {
		return err
	}
	lines, err := distributionLines(data)
	if err != nil {
		return err
	}
	found := 0
	for _, line := range lines {
		if strings.EqualFold(line, w.Distribution) {
			found++
		}
	}
	if found != 1 {
		return t3.ErrUnavailable
	}
	return nil
}

func (w WSL) inspect(ctx context.Context, command string, args []string, input []byte) ([]byte, error) {
	if err := w.running(ctx); err != nil {
		return nil, err
	}
	arguments := []string{"--distribution", w.Distribution, "--exec", command}
	arguments = append(arguments, args...)
	return w.run(ctx, arguments, input)
}

// Match never launches an inspection for an observed stopped or unreadable
// distribution. A stop after preflight can restart it; no termination follows.
func (w WSL) Match(ctx context.Context, name, executable string) model.Observation {
	unknown := model.Observation{State: model.Unknown, ErrorCode: "incomplete"}
	if (name == "") == (executable == "") || strings.ContainsFunc(name+executable, unicode.IsControl) {
		return unknown
	}
	data, err := w.inspect(ctx, "/usr/bin/id", []string{"-u"}, nil)
	if err != nil {
		return unknown
	}
	text := strings.TrimSuffix(string(data), "\n")
	uid, err := strconv.ParseUint(text, 10, 32)
	if err != nil || text != strconv.FormatUint(uid, 10) || !bytes.HasSuffix(data, []byte("\n")) {
		return unknown
	}
	field, selector, own := "comm", name, "ps"
	if executable != "" {
		if !strings.HasPrefix(executable, "/") {
			return unknown
		}
		field, selector, own = "exe", path.Clean(executable), "/usr/bin/ps"
	}
	data, err = w.inspect(ctx, "/usr/bin/ps", []string{"-ww", "-u", text, "-o", field + "="}, nil)
	if err != nil {
		return unknown
	}
	if !utf8.Valid(data) || len(data) > t3.MaxBytes || !bytes.HasSuffix(data, []byte("\n")) {
		return unknown
	}
	rows := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	self, active, incomplete := false, false, false
	for _, row := range rows {
		if !self && row == own {
			self = true
			continue
		} // Exclude precisely one inspecting ps.
		if row == "" || row == "-" || strings.ContainsAny(row, "?\x00") || strings.ContainsFunc(row, unicode.IsControl) || (field == "exe" && (!strings.HasPrefix(row, "/") || strings.HasSuffix(row, " (deleted)"))) {
			incomplete = true
			continue
		}
		if row == selector {
			active = true
		}
	}
	if !self {
		return unknown
	}
	if active {
		return model.Observation{State: model.Active}
	}
	if incomplete {
		return unknown
	}
	return model.Observation{State: model.Inactive}
}

type wslHTTP struct {
	w        WSL
	endpoint string
}

func curlValue(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r") {
		return "", t3.ErrSchema
	}
	value = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\t", "\\t").Replace(value)
	return "\"" + value + "\"", nil
}
func (w WSL) HTTPTransport(endpoint string) (http.RoundTripper, error) {
	if _, err := t3.Endpoint(endpoint); err != nil {
		return nil, err
	}
	return &wslHTTP{w: w, endpoint: endpoint}, nil
}
func (t *wslHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() != t.endpoint || (req.Method != "POST" && req.Method != "DELETE") {
		return nil, t3.ErrIdentity
	}
	u, _ := t3.Endpoint(t.endpoint)
	host := u.Hostname()
	if host == "localhost" {
		host = "127.0.0.1"
	}
	var configuration strings.Builder
	configuration.WriteString("silent\nshow-error\nraw\ninclude\nhttp1.1\ngloboff\nmax-time = 2\nmax-filesize = 1048576\nretry = 0\nproxy = \"\"\nnoproxy = \"*\"\nproto = \"=http,https\"\nheader = \"Expect:\"\n")
	values := [][2]string{{"url", t.endpoint}, {"request", req.Method}, {"connect-to", net.JoinHostPort(u.Hostname(), u.Port()) + ":" + net.JoinHostPort(host, u.Port())}}
	allowed := map[string]bool{"accept": true, "content-type": true, "authorization": true, "mcp-session-id": true, "mcp-protocol-version": true, "user-agent": true}
	for key, headers := range req.Header {
		if !allowed[strings.ToLower(key)] {
			return nil, t3.ErrSchema
		}
		for _, v := range headers {
			if strings.ContainsAny(v, "\r\n\x00") {
				return nil, t3.ErrSchema
			}
			values = append(values, [2]string{"header", key + ": " + v})
		}
	}
	if req.Body != nil {
		body, err := io.ReadAll(io.LimitReader(req.Body, t3.MaxBytes+1))
		req.Body.Close()
		if err != nil || len(body) > t3.MaxBytes {
			return nil, t3.ErrIncomplete
		}
		values = append(values, [2]string{"data-binary", string(body)})
	}
	for _, pair := range values {
		value, err := curlValue(pair[1])
		if err != nil {
			return nil, err
		}
		configuration.WriteString(pair[0] + " = " + value + "\n")
	}
	input := []byte(configuration.String())
	defer clear(input)
	data, err := t.w.inspect(req.Context(), "/usr/bin/curl", []string{"-q", "--config", "-"}, input)
	if err != nil {
		return nil, t3.ErrIdentity
	}
	end := bytes.Index(data, []byte("\r\n\r\n"))
	if end < 0 || end > 16<<10 || len(data) > t3.MaxBytes+(16<<10) {
		return nil, t3.ErrIncomplete
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(data)), req)
	if err != nil {
		return nil, t3.ErrSchema
	}
	return response, nil
}

// VerifyT3Owner checks the actual selected loopback listener's current-user SID.
// Native endpoints cannot silently bind a WSL relay, or vice versa.
func VerifyT3Owner(ctx context.Context, b t3.Binding) error {
	u, err := t3.Endpoint(b.Endpoint)
	if err != nil {
		return err
	}
	port, _ := strconv.Atoi(u.Port())
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() == "localhost" {
		ip = net.IPv4(127, 0, 0, 1)
	}
	family, rowSize, portOffset, pidOffset := 2, 24, 8, 20
	if ip.To4() == nil {
		family, rowSize, portOffset, pidOffset = 23, 56, 20, 52
	}
	dir, err := systemDirectory()
	if err != nil {
		return err
	}
	proc := syscall.NewLazyDLL(filepath.Join(dir, "iphlpapi.dll")).NewProc("GetExtendedTcpTable")
	var size uint32
	result, _, _ := proc.Call(0, uintptr(unsafe.Pointer(&size)), 0, uintptr(family), 3, 0)
	if result != 122 && result != 0 {
		return t3.ErrIdentity
	}
	if size < 4 || size > t3.MaxBytes {
		return t3.ErrIdentity
	}
	var data []byte
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if size < 4 || size > t3.MaxBytes {
			return t3.ErrIdentity
		}
		data = make([]byte, size)
		result, _, _ = proc.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(family), 3, 0)
		runtime.KeepAlive(data)
		if result != 122 {
			break
		}
	}
	if result != 0 || size < 4 || int(size) > len(data) {
		return t3.ErrIdentity
	}
	count := int(binary.LittleEndian.Uint32(data))
	if count > (int(size)-4)/rowSize {
		return t3.ErrIdentity
	}
	owners := map[uint32]bool{}
	for i := 0; i < count; i++ {
		row := data[4+i*rowSize : 4+(i+1)*rowSize]
		address := net.IP(row[4:8])
		if family == 23 {
			address = net.IP(row[:16])
		}
		if (address.IsUnspecified() || address.Equal(ip)) && int(binary.BigEndian.Uint16(row[portOffset:])) == port {
			owners[binary.LittleEndian.Uint32(row[pidOffset:])] = true
		}
	}
	if len(owners) != 1 {
		return t3.ErrIdentity
	}
	sid, err := currentSID()
	if err != nil {
		return t3.ErrIdentity
	}
	for pid := range owners {
		handle, err := syscall.OpenProcess(0x1000, false, pid)
		if err != nil {
			return t3.ErrIdentity
		}
		owner, e := processSID(handle)
		name, pathErr := processPath(handle)
		syscall.CloseHandle(handle)
		if e != nil || pathErr != nil || owner != sid {
			return t3.ErrIdentity
		}
		relay := strings.EqualFold(filepath.Base(name), "wslrelay.exe") || strings.EqualFold(filepath.Base(name), "wslhost.exe")
		if relay != (b.WSLDistribution != "") {
			return t3.ErrIdentity
		}
	}
	return nil
}

func VerifyT3(ctx context.Context, b t3.Binding, token string) error {
	if err := VerifyT3Owner(ctx, b); err != nil {
		return err
	}
	var carrier http.RoundTripper
	if b.WSLDistribution != "" {
		var err error
		carrier, err = (WSL{Distribution: b.WSLDistribution}).HTTPTransport(b.Endpoint)
		if err != nil {
			return err
		}
	}
	return t3.VerifyIdentity(ctx, b, token, carrier)
}

func LoadT3Bindings(dir string) ([]byte, error) {
	name := filepath.Join(dir, "environments.json")
	if err := noRedirect(name); err != nil {
		return nil, err
	}
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > t3.MaxBytes {
		return nil, t3.ErrUnavailable
	}
	if protectedDirectory(dir, false) != nil || privateACL(name) != nil {
		return nil, t3.ErrUnavailable
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, t3.ErrUnavailable
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, t3.MaxBytes+1))
	if err != nil || len(data) > t3.MaxBytes {
		return nil, t3.ErrUnavailable
	}
	return data, nil
}
func SaveT3Bindings(dir string, data []byte) error {
	if len(data) == 0 || len(data) > t3.MaxBytes || protectedDirectory(dir, true) != nil {
		return t3.ErrUnavailable
	}
	name := filepath.Join(dir, "environments.json")
	if noRedirect(name) != nil {
		return t3.ErrUnavailable
	}
	if info, err := os.Lstat(name); err == nil {
		if !info.Mode().IsRegular() || privateACL(name) != nil {
			return t3.ErrUnavailable
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return t3.ErrUnavailable
	}
	file, err := os.CreateTemp(dir, ".snoozap-environments-*")
	if err != nil {
		return t3.ErrUnavailable
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closed := file.Close()
	if err != nil || closed != nil || privateACL(temporary) != nil {
		return t3.ErrUnavailable
	}
	from, _ := syscall.UTF16PtrFromString(temporary)
	to, _ := syscall.UTF16PtrFromString(name)
	ok, _, _ := kernel.NewProc("MoveFileExW").Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), 9)
	runtime.KeepAlive(from)
	runtime.KeepAlive(to)
	if ok == 0 {
		return t3.ErrUnavailable
	}
	return nil
}
