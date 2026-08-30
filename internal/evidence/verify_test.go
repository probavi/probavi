package evidence

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// TestReadLineIsBounded pins the property that makes verification safe on a
// file someone else produced: no single line may pull more than a record's
// worth of bytes into memory before the size rule applies.
func TestReadLineIsBounded(t *testing.T) {
	tests := []struct {
		name string
		// bufSize is the reader's buffer; a line longer than it must still
		// be assembled across reads, which is what the loop is for.
		bufSize        int
		input          string
		wantLine       string
		wantTerminated bool
		wantTooLong    bool
	}{
		{"short line", readChunkBytes, "abc\n", "abc", true, false},
		{"empty line", readChunkBytes, "\n", "", true, false},
		{"torn tail", readChunkBytes, "abc", "abc", false, false},
		{"empty input", readChunkBytes, "", "", false, false},
		{"line spanning several reads", 16, strings.Repeat("x", 1000) + "\n",
			strings.Repeat("x", 1000), true, false},
		{"exactly at the cap", readChunkBytes, strings.Repeat("x", MaxRecordBytes-1) + "\n",
			strings.Repeat("x", MaxRecordBytes-1), true, false},
		{"one byte over the cap", readChunkBytes, strings.Repeat("x", MaxRecordBytes+1) + "\n", "", false, true},
		{"no newline at all", readChunkBytes, strings.Repeat("x", MaxRecordBytes*2), "", false, true},
		{"over the cap with a tiny buffer", 16, strings.Repeat("x", MaxRecordBytes+1), "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			br := bufio.NewReaderSize(strings.NewReader(tt.input), tt.bufSize)
			line, terminated, tooLong, err := readLine(br, MaxRecordBytes)
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("readLine: %v", err)
			}
			if string(line) != tt.wantLine || terminated != tt.wantTerminated || tooLong != tt.wantTooLong {
				t.Errorf("readLine = (%d bytes, terminated %v, tooLong %v), want (%d bytes, %v, %v)",
					len(line), terminated, tooLong, len(tt.wantLine), tt.wantTerminated, tt.wantTooLong)
			}
		})
	}
}

// TestVerifyRefusesAnEndlessLine is the reason readLine exists: without a
// cap this input is bounded only by available memory. With one, it is a
// verdict.
func TestVerifyRefusesAnEndlessLine(t *testing.T) {
	res, err := Verify(strings.NewReader(strings.Repeat("x", MaxRecordBytes+64)), Keyring{}, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != StatusInvalid || res.FailedLine != 1 {
		t.Errorf("status = %v line = %d, want INVALID at line 1", res.Status, res.FailedLine)
	}
	if !strings.Contains(res.Reason, "maximum canonical size") {
		t.Errorf("reason = %q, want it to name the size rule", res.Reason)
	}
}

// TestReadLineSurfacesReadErrors keeps a genuine I/O failure distinguishable
// from an over-long line: one is the caller's problem, the other a verdict.
func TestReadLineSurfacesReadErrors(t *testing.T) {
	want := errors.New("disk on fire")
	br := bufio.NewReaderSize(iotest.ErrReader(want), readChunkBytes)
	if _, _, _, err := readLine(br, MaxRecordBytes); !errors.Is(err, want) {
		t.Errorf("err = %v, want the underlying read error", err)
	}
	if _, err := Verify(iotest.ErrReader(want), Keyring{}, nil); !errors.Is(err, want) {
		t.Errorf("Verify err = %v, want the underlying read error", err)
	}
}
