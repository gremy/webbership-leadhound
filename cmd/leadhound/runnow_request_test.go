package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSendNowFile writes text as the Send now request file under data, mtime mt.
func writeSendNowFile(t *testing.T, data, text string, mt time.Time) {
	t.Helper()
	dir := filepath.Join(data, runRequestDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sendNowAction)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// TestSendNowRequestFile (amendment 8): the send service reads a time or an empty file as
// today's Send now, a JSON line as a manual batch's, and refuses JSON that does not parse,
// names no batch or carries more than the batch; the console's state reads the request time
// from each, the mtime when there is none.
func TestSendNowRequestFile(t *testing.T) {
	mt := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	at := time.Date(2026, 10, 31, 9, 5, 0, 123, time.UTC)
	for _, tc := range []struct {
		name, text string
		kind       string // "" refused
		batch      int64
		asked      time.Time
	}{
		{"time", at.Format(time.RFC3339Nano) + "\n", sendRequestLegacy, 0, at},
		{"empty", "", sendRequestLegacy, 0, mt},
		{"torn time", "2026-10-3", sendRequestLegacy, 0, mt},
		{"manual", `{"batch_id":42,"at":"` + at.Format(time.RFC3339Nano) + `"}` + "\n", sendRequestManual, 42, at},
		{"manual without at", `{"batch_id":7}`, sendRequestManual, 7, mt},
		{"malformed", `{"batch_id":42,`, "", 0, mt},
		{"batch 0", `{"batch_id":0,"at":"` + at.Format(time.RFC3339Nano) + `"}`, "", 0, at},
		{"negative batch", `{"batch_id":-3}`, "", 0, mt},
		{"no batch", `{"at":"` + at.Format(time.RFC3339Nano) + `"}`, "", 0, at},
		{"batch as text", `{"batch_id":"42"}`, "", 0, mt},
		{"codes in the file", `{"batch_id":42,"codes":["ramp"]}`, "", 0, mt},
		{"two requests", `{"batch_id":42} {"batch_id":43}`, "", 0, mt},
		{"oversized", `{"batch_id":42,"at":"` + strings.Repeat(" ", maxSendNowRequest) + `"}`, "", 0, mt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := t.TempDir()
			writeSendNowFile(t, data, tc.text, mt)
			batch, kind, err := readSendNowRequest(data)
			switch {
			case tc.kind == "" && err == nil:
				t.Errorf("read = %d %q, want refused", batch, kind)
			case tc.kind != "" && (err != nil || kind != tc.kind || batch != tc.batch):
				t.Errorf("read = %d %q %v, want %d %q", batch, kind, err, tc.batch, tc.kind)
			}
			got, err := readRunRequest(data, sendNowAction)
			if err != nil || got == nil || !got.Equal(tc.asked) {
				t.Errorf("request time = %v, %v; want %v", got, err, tc.asked)
			}
		})
	}
	if _, _, err := readSendNowRequest(t.TempDir()); err == nil {
		t.Error("no request file: want refused")
	}
	if _, _, err := readSendNowRequest(""); err == nil {
		t.Error("no data dir: want refused")
	}
}
