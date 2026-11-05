package agent

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"testing"
)

// Padding exercises frame limits without storing multi-megabyte corpus files.
type framePaddingReader struct{}

func (framePaddingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

type frameCountingReader struct {
	io.Reader
	n int
}

func (r *frameCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += n
	return n, err
}

func FuzzReadFrameBounded(f *testing.F) {
	for _, seed := range []string{
		`{"id":"dev1","op":"stdin_ack"}`, `{"id":"dev1","op":"output_ack"}`,
		`{"id":"dev1","op":"cancel"}`, `{"id":"dev1","op":"stdin","eof":true}`,
		`{"id":"dev1","op":"exec","cmd":["true"]}`, `{"id":"dev1","payload":{"x":1}}`,
		`{"id":"dev1","timeout_s":18446744073709551616}`, `{"id":[]}`, "{", "null", "\x00\xff\n",
	} {
		f.Add([]byte(seed), uint32(0), uint16(16))
	}
	f.Add([]byte("{\"id\":\"dev1\"}\n"), uint32(MaxFrameBytes), uint16(4096))
	f.Add([]byte("{\"id\":\"dev1\"}\n"), uint32(MaxFrameBytes+1), uint16(1))
	f.Fuzz(func(t *testing.T, data []byte, padding uint32, bufferSize uint16) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		padding %= MaxFrameBytes + 2
		input := &frameCountingReader{Reader: io.MultiReader(io.LimitReader(framePaddingReader{}, int64(padding)), bytes.NewReader(data))}
		r := bufio.NewReaderSize(input, int(bufferSize)%4096+16)
		var decoded Frame
		err := readFrame(r, &decoded)
		if input.n > MaxFrameBytes+r.Size() {
			t.Fatalf("read beyond frame limit: %d", input.n)
		}
		if err != nil {
			return
		}
		if decoded.ID == "" {
			t.Fatal("accepted missing ID")
		}
		var encoded bytes.Buffer
		if err := newFrameWriter(&encoded).write(&decoded); err != nil {
			t.Fatal(err)
		}
		var roundTrip Frame
		if err := readFrame(bufio.NewReader(&encoded), &roundTrip); err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(decoded)
		after, _ := json.Marshal(roundTrip)
		if !bytes.Equal(before, after) {
			t.Fatal("codec round trip changed frame")
		}
	})
}

func BenchmarkFrameCodec(b *testing.B) {
	for _, tc := range []struct {
		name  string
		frame Frame
	}{
		{"ack", Frame{ID: "dev1", Op: OpOutputAck}},
		{"exec", Frame{ID: "dev1", Op: OpExec, Cmd: []string{"echo", "ok"}, TimeoutS: 30}},
		{"stdout32K", Frame{ID: "dev1", Stream: StreamStdout, DataB64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 32<<10))}},
	} {
		var encoded bytes.Buffer
		if err := newFrameWriter(&encoded).write(&tc.frame); err != nil {
			b.Fatal(err)
		}
		payload := append([]byte(nil), encoded.Bytes()...)
		b.Run(tc.name+"/read", func(b *testing.B) {
			src := bytes.NewReader(payload)
			r := bufio.NewReaderSize(src, 64<<10)
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				src.Reset(payload)
				r.Reset(src)
				var out Frame
				if err := readFrame(r, &out); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(tc.name+"/write", func(b *testing.B) {
			w := newFrameWriter(io.Discard)
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				if err := w.write(&tc.frame); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
