package schemainfer

import (
	"errors"
	"strings"
	"testing"
)

func collectEntries(t *testing.T, text string) ([]string, []int, error) {
	t.Helper()
	var got []string
	var lines []int
	err := ForEachEntry(strings.NewReader(text), func(b []byte, line int) error {
		got = append(got, string(b))
		lines = append(lines, line)
		return nil
	})
	return got, lines, err
}

func TestForEachEntryLines(t *testing.T) {
	got, lines, err := collectEntries(t, "  \n{\"a\":1}\n\n  {\"b\":2}  \r\n")
	if err != nil || strings.Join(got, "|") != `{"a":1}|{"b":2}` || lines[0] != 2 || lines[1] != 4 {
		t.Fatalf("got %v lines %v err %v", got, lines, err)
	}
}

func TestForEachEntryArray(t *testing.T) {
	got, lines, err := collectEntries(t, "\n [ {\"a\":1},\n {\"b\":{\"c\":[1,2]}} ]\n")
	if err != nil || strings.Join(got, "|") != `{"a":1}|{"b":{"c":[1,2]}}` || lines[1] != 2 {
		t.Fatalf("got %v lines %v err %v", got, lines, err)
	}
	if _, _, err := collectEntries(t, `[{"a":1}, {"b":`); err == nil || !strings.Contains(err.Error(), "array element 2") {
		t.Fatalf("broken array: %v", err)
	}
}

func TestForEachEntryStopsOnCallbackError(t *testing.T) {
	stop := errors.New("stop")
	n := 0
	err := ForEachEntry(strings.NewReader("{}\n{}\n{}\n"), func([]byte, int) error {
		n++
		return stop
	})
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("err %v after %d entries", err, n)
	}
	if got, _, err := collectEntries(t, ""); err != nil || len(got) != 0 {
		t.Fatalf("empty input: %v %v", got, err)
	}
}
