package schedule

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNextTimezoneDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ spec, after, want string }{
		{"0 8 * * *", "2026-03-07T14:00:00-05:00", "2026-03-08T08:00:00-04:00"},
		{"30 2 * * *", "2026-03-07T14:00:00-05:00", "2026-03-09T02:30:00-04:00"},
		{"30 1 * * *", "2026-11-01T01:30:00-04:00", "2026-11-01T01:30:00-05:00"},
		{"at 2026-10-24 08:00", "2026-10-23T12:00:00-04:00", "2026-10-24T08:00:00-04:00"},
		{"every 30m", "2026-03-08T01:45:00-05:00", "2026-03-08T03:15:00-04:00"},
	}
	for _, tc := range cases {
		after, _ := time.Parse(time.RFC3339, tc.after)
		want, _ := time.Parse(time.RFC3339, tc.want)
		next, err := Next(tc.spec, after, loc)
		if err != nil || !next.Equal(want) {
			t.Errorf("%s: %s %v, want %s", tc.spec, next, err, want)
		}
	}
	for _, spec := range []string{"at 2026-03-08 02:30", "every 0m", "* * *", "0 0 0 * * *", "CRON_TZ=UTC 0 8 * * *", "0 0 31 2 *"} {
		if _, err := Next(spec, time.Date(2026, 1, 1, 0, 0, 0, 0, loc), loc); err == nil {
			t.Errorf("accepted %s", spec)
		}
	}
}
func TestAdvanceAndStoreRecovery(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	task := Task{ID: "one", When: "every 30m", Next: now.Add(-1000 * time.Hour), Running: true}
	next, err := Advance(task, now, time.UTC)
	if err != nil || !next.Equal(now.Add(30*time.Minute)) {
		t.Fatal(next, err)
	}
	path := filepath.Join(t.TempDir(), "data", "schedules.json")
	st, err := Open(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Save([]Task{task}); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path, now)
	if err != nil {
		t.Fatal(err)
	}
	ts := st.List()
	if ts[0].Running || ts[0].Last == nil || ts[0].Last.OK {
		t.Fatal(ts)
	}
	ts[0].Name = "changed"
	if st.List()[0].Name != "" {
		t.Fatal("not cloned")
	}
	for i := 0; i < 20; i++ {
		Record(&ts[0], Result{At: now, OK: true})
	}
	if len(ts[0].Results) != 10 {
		t.Fatal("unbounded results")
	}
	if err = os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(path, now); err == nil {
		t.Fatal("corruption ignored")
	}
}

func TestLatestDueOnlyRecentOccurrence(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 10, 0, 0, time.UTC)
	for _, tc := range []struct {
		spec string
		want time.Time
	}{
		{"every 30m", now.Add(-10 * time.Minute)}, {"0 12 * * *", now.Add(-10 * time.Minute)}, {"0 0 * * *", now.Add(-48 * time.Hour)},
	} {
		task := Task{When: tc.spec, Next: now.Add(-48*time.Hour - 10*time.Minute)}
		if tc.spec == "0 0 * * *" {
			task.Next = tc.want
		}
		if got := LatestDue(task, now, time.UTC); !got.Equal(tc.want) {
			t.Fatal(tc.spec, got, tc.want)
		}
	}
}
