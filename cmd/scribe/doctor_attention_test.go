package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFailureStreak(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		statuses []string
		count    int
		status   checkStatus
	}{
		{"transient", []string{"error"}, 1, statusWarn},
		{"below threshold", []string{"error", "degraded"}, 2, statusWarn},
		{"skips ignored", []string{"error", "skipped", "degraded", "error"}, 3, statusFail},
		{"recovered", []string{"error", "error", "error", "ok"}, 0, statusWarn},
		{"reset", []string{"error", "error", "ok", "error"}, 1, statusWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			var lines []string
			for i, status := range tc.statuses {
				lines = append(lines, fmt.Sprintf(`{"command":"dream","args":["--hot"],"status":%q,"timestamp":%q,"error":"bad model"}`, status, now.Add(time.Duration(i-10)*time.Hour).Format(time.RFC3339)))
			}
			// Reverse physical order: append order is not timestamp order.
			for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
				lines[i], lines[j] = lines[j], lines[i]
			}
			lines = append(lines, `{"command":"dream","status":"ok","timestamp":"2026-09-09T11:00:00Z"}`, `{"command":"doctor","status":"error","timestamp":"2026-09-09T11:00:00Z"}`)
			writeRunsFile(t, root, "2026-09-09", strings.Join(lines, "\n")+"\n")
			got, err := loadRunErrors(root, now.Add(-30*24*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			streak := got["dream --hot"]
			if len(got) != 1 || streak.Count != tc.count {
				t.Fatalf("got %#v", got)
			}
			if tc.count == 3 && !streak.First.Equal(now.Add(-10*time.Hour)) {
				t.Fatalf("first = %v", streak.First)
			}
			checks := checkRecentErrors(root, now, 24*time.Hour)
			if len(checks) != 1 || checks[0].Status != tc.status {
				t.Fatalf("checks = %#v", checks)
			}
		})
	}
}

func TestFailureScanWindow(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	writeRunsFile(t, root, "2026-07-01", strings.Repeat("x", 2*1024*1024)) // must not even scan this file
	writeRunsFile(t, root, "2026-09-01", `{"command":"sync","status":"error","timestamp":"2026-09-01T12:00:00Z"}`+"\n")
	checks := checkRecentErrors(root, now, 24*time.Hour)
	if len(checks) != 1 || checks[0].Name != "sync" || !strings.Contains(checks[0].Detail, "1 consecutive") {
		t.Fatalf("checks = %#v", checks)
	}
	writeRunsFile(t, root, "2026-09-02", `{"command":"sync","status":"ok","timestamp":"2026-09-02T12:00:00Z"}`+"\n")
	checks = checkRecentErrors(root, now, 24*time.Hour)
	if len(checks) != 1 || checks[0].Status != statusOK {
		t.Fatalf("recovered = %#v", checks)
	}
}

func TestAttentionReportLifecycle(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "NEEDS-ATTENTION.md")
	fail := []check{{Section: "errors", Name: "dream --hot", Status: statusFail, Detail: "3 consecutive runs", Fix: "inspect output/runs/"}, {Name: "optional", Status: statusWarn}}
	if err := writeAttentionReport(root, fail); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "3 consecutive runs") || strings.Contains(string(data), "optional") {
		t.Fatalf("report = %s", data)
	}
	if err := writeAttentionReport(root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("report not removed: %v", err)
	}
	if err := os.WriteFile(path, []byte("my notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, checks := range [][]check{nil, fail} {
		if err := writeAttentionReport(root, checks); err == nil {
			t.Fatal("user-owned report must be preserved")
		}
	}
}

func TestAttentionCommandAndSchedule(t *testing.T) {
	if commandIsReadOnly(parseForTest(t, "doctor", "--write-attention")) {
		t.Fatal("explicit write must record its run")
	}
	if !commandIsReadOnly(parseForTest(t, "doctor")) {
		t.Fatal("plain doctor must remain read-only")
	}
	cmd := DoctorCmd{WriteAttention: true, Section: "errors", ErrorWindow: 24 * time.Hour}
	if err := cmd.Run(); err == nil {
		t.Fatal("partial report could erase failures from other sections")
	}
	for _, job := range scribeJobs("/test/scribe") {
		if job.Name == "doctor" {
			if job.Command != "/test/scribe each -- doctor --write-attention" || job.Schedule.Calendar[0].Hour != 7 || job.Schedule.Calendar[0].Minute != 30 {
				t.Fatalf("job = %#v", job)
			}
			return
		}
	}
	t.Fatal("missing scheduled delivery")
}
