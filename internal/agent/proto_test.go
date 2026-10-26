package agent

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := newFrameWriter(&buf)
	in := &Frame{ID: "x1", Op: OpExec, Cmd: []string{"echo", "ok"}, TimeoutS: 5}
	if err := w.write(in); err != nil {
		t.Fatal(err)
	}
	var out Frame
	if err := readFrame(bufio.NewReader(&buf), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != in.ID || out.Op != in.Op || len(out.Cmd) != 2 || out.Cmd[1] != "ok" || out.TimeoutS != 5 {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestReadFrameErrors(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("{\"op\":\"ping\"}\n"))
	var f Frame
	if err := readFrame(r, &f); err == nil {
		t.Fatal("want error for missing id")
	}

	r = bufio.NewReader(strings.NewReader("not json\n"))
	if err := readFrame(r, &f); err == nil {
		t.Fatal("want error for bad json")
	}
}

func TestReadFrameUnterminated(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("{\"id\":\"a\",\"done\":true,\"exit_code\":3}"))
	var f Frame
	if err := readFrame(r, &f); err != nil {
		t.Fatal(err)
	}
	if f.ExitCode != 3 {
		t.Fatalf("got %+v", f)
	}
}

func TestDataB64Payload(t *testing.T) {
	raw := []byte("hello\x00world")
	f := Frame{ID: "a", Stream: StreamStdout, DataB64: base64.StdEncoding.EncodeToString(raw)}
	var buf bytes.Buffer
	if err := newFrameWriter(&buf).write(&f); err != nil {
		t.Fatal(err)
	}
	var out Frame
	if err := readFrame(bufio.NewReader(&buf), &out); err != nil {
		t.Fatal(err)
	}
	dec, err := base64.StdEncoding.DecodeString(out.DataB64)
	if err != nil || !bytes.Equal(dec, raw) {
		t.Fatalf("payload mismatch: %v %q", err, dec)
	}
}

func TestReadFrameRejectsOversizedInputBeforeEOF(t *testing.T) {
	for _, suffix := range []string{"", "\n"} {
		t.Run(fmt.Sprintf("newline=%t", suffix != ""), func(t *testing.T) {
			input := strings.NewReader(strings.Repeat("x", 2*MaxFrameBytes) + suffix)
			r := bufio.NewReaderSize(input, 64<<10)
			var f Frame
			if err := readFrame(r, &f); err == nil || !strings.Contains(err.Error(), "frame too large") {
				t.Fatalf("want size error, got %v", err)
			}
			if read := 2*MaxFrameBytes + len(suffix) - input.Len(); read > MaxFrameBytes+r.Size() {
				t.Fatalf("read %d bytes before rejecting oversized frame", read)
			}
		})
	}
}

func TestReadFrameSizeBoundary(t *testing.T) {
	for _, terminated := range []bool{false, true} {
		suffix := ""
		if terminated {
			suffix = "\n"
		}
		frame := `{"id":"a","data_b64":"` + strings.Repeat("x", MaxFrameBytes-len(`{"id":"a","data_b64":""}`)-len(suffix)) + `"}` + suffix
		var f Frame
		r := bufio.NewReaderSize(strings.NewReader(frame), 64<<10)
		if err := readFrame(r, &f); err != nil {
			t.Fatalf("terminated=%t: %v", terminated, err)
		}
		if err := readFrame(r, &f); err != io.EOF {
			t.Fatalf("want EOF, got %v", err)
		}
	}
}
