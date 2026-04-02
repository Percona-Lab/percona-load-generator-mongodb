package benchmark

import (
	"bytes"
	"testing"
	"unicode"
)

func TestGeneratePayload(t *testing.T) {
	tests := []struct {
		name string
		size int
		want int
	}{
		{name: "zero_uses_default", size: 0, want: 1024},
		{name: "negative_uses_default", size: -10, want: 1024},
		{name: "custom_size", size: 64, want: 64},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := GeneratePayload(tc.size)
			if len(payload) != tc.want {
				t.Fatalf("expected payload size %d, got %d", tc.want, len(payload))
			}
		})
	}
}

func TestGenerateRandomString(t *testing.T) {
	if got := GenerateRandomString(0); got != "" {
		t.Fatalf("expected empty string for size 0, got %q", got)
	}

	s := GenerateRandomString(32)
	if len(s) != 32 {
		t.Fatalf("expected len 32, got %d", len(s))
	}
	for _, r := range s {
		if !unicode.IsDigit(r) && !unicode.IsLetter(r) {
			t.Fatalf("expected alphanumeric characters only, got %q", string(r))
		}
	}
}

func TestBuildBatchArray(t *testing.T) {
	template := []byte{0x11, 0x22, 0x33}
	magic := []byte{0x22}
	arr, offsets, err := buildBatchArray(template, 4, magic)
	if err != nil {
		t.Fatalf("buildBatchArray() error = %v", err)
	}
	if len(offsets) != 4 {
		t.Fatalf("expected 4 offsets, got %d", len(offsets))
	}
	for _, off := range offsets {
		if off < 0 || off >= len(arr) {
			t.Fatalf("invalid offset %d", off)
		}
		if !bytes.Equal(arr[off:off+1], magic) {
			t.Fatalf("offset %d does not point to magic byte", off)
		}
	}

	_, _, err = buildBatchArray(template, 1, []byte{0xFF})
	if err == nil {
		t.Fatalf("expected error when magic byte is absent")
	}

	arr, offsets, err = buildBatchArray(template, 0, magic)
	if err != nil {
		t.Fatalf("buildBatchArray(count=0) error = %v", err)
	}
	if len(offsets) != 0 {
		t.Fatalf("expected no offsets for empty batch, got %d", len(offsets))
	}
	if len(arr) == 0 {
		t.Fatalf("expected valid BSON bytes for empty batch")
	}
}
