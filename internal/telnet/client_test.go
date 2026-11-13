package telnet

import (
	"bytes"
	"io"
	"testing"
)

type wire struct {
	in    *bytes.Reader
	out   bytes.Buffer
	chunk int
}

func (w *wire) Read(b []byte) (int, error) {
	if w.chunk > 0 && len(b) > w.chunk {
		b = b[:w.chunk]
	}
	return w.in.Read(b)
}
func (w *wire) Write(b []byte) (int, error) { return w.out.Write(b) }

func TestFragmentedNegotiation(t *testing.T) {
	// A switch requests echo, terminal type and window size before its login prompt.
	input := []byte{iac, will, 1, iac, will, 3, iac, do, 24, iac, do, 31,
		iac, sb, 24, 1, iac, se, iac, will, 42}
	input = append(input, []byte("Password: ")...)
	input = append(input, iac, iac)
	for _, chunk := range []int{1, 2, 7, 1024} {
		w := &wire{in: bytes.NewReader(input), chunk: chunk}
		c := New(w)
		if err := c.Resize(255, 40); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(c)
		if err != nil || !bytes.Equal(got, append([]byte("Password: "), 255)) {
			t.Fatalf("chunk %d output %q: %v", chunk, got, err)
		}
		want := []byte{iac, do, 1, iac, do, 3, iac, will, 24, iac, will, 31,
			iac, sb, 31, 0, 255, 255, 0, 40, iac, se,
			iac, sb, 24, 0, 'x', 't', 'e', 'r', 'm', iac, se, iac, dont, 42}
		if !bytes.Equal(w.out.Bytes(), want) {
			t.Fatalf("chunk %d negotiation %x, want %x", chunk, w.out.Bytes(), want)
		}
		w.out.Reset()
		if err := c.Send(append([]byte("show version\r\n"), iac)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(w.out.Bytes(), append([]byte("show version\r\n"), iac, iac)) {
			t.Fatalf("wire input %q", w.out.Bytes())
		}
	}
}

func TestRepeatedAndDisabledOptions(t *testing.T) {
	w := &wire{in: bytes.NewReader([]byte{iac, do, 31, iac, do, 31, iac, dont, 31, iac, do, 1, '>'})}
	c := New(w)
	if _, err := io.ReadAll(c); err != nil {
		t.Fatal(err)
	}
	want := []byte{iac, will, 31, iac, sb, 31, 0, 100, 0, 30, iac, se, iac, wont, 31, iac, wont, 1}
	if !bytes.Equal(w.out.Bytes(), want) {
		t.Fatalf("negotiation %x", w.out.Bytes())
	}
	w.out.Reset()
	if err := c.Resize(80, 24); err != nil || w.out.Len() != 0 {
		t.Fatal("disabled NAWS must not send sizes")
	}
}
