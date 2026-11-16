package probe

import (
	"bytes"
	"io"
	"testing"
)

// io.Copy skips Write when the destination has ReadFrom, so limitedBuffer must never get one
// (embedding bytes.Buffer would add it silently)
func TestLimitedBufferHasNoReadFrom(t *testing.T) {
	if _, ok := any(&limitedBuffer{}).(io.ReaderFrom); ok {
		t.Fatal("limitedBuffer implements io.ReaderFrom: io.Copy would bypass the size limit")
	}
}

func TestLimitedBufferThroughIOCopy(t *testing.T) {
	b := &limitedBuffer{max: 10}
	n, err := io.Copy(b, bytes.NewReader(make([]byte, 1000)))
	if err != nil || n != 1000 {
		t.Fatalf("io.Copy = %d, %v; want all 1000 bytes accepted so the pipe keeps draining", n, err)
	}
	if len(b.Bytes()) != 10 || !b.overflow {
		t.Errorf("kept %d bytes, overflow %v; want 10 and true", len(b.Bytes()), b.overflow)
	}
}
