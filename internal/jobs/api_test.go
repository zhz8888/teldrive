package jobs

import (
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"
)

func TestRetryRequeuesJob(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		row  *rivertype.JobRow
		want bool
	}{
		{name: "running", row: &rivertype.JobRow{State: rivertype.JobStateRunning}, want: false},
		{name: "available and due", row: &rivertype.JobRow{State: rivertype.JobStateAvailable, ScheduledAt: time.Now().Add(-time.Minute)}, want: false},
		{name: "available in the future", row: &rivertype.JobRow{State: rivertype.JobStateAvailable, ScheduledAt: time.Now().Add(time.Minute)}, want: true},
		{name: "completed", row: &rivertype.JobRow{State: rivertype.JobStateCompleted}, want: true},
		{name: "discarded", row: &rivertype.JobRow{State: rivertype.JobStateDiscarded}, want: true},
		{name: "cancelled", row: &rivertype.JobRow{State: rivertype.JobStateCancelled}, want: true},
		{name: "retryable", row: &rivertype.JobRow{State: rivertype.JobStateRetryable}, want: true},
	} {
		if got := retryRequeuesJob(test.row); got != test.want {
			t.Errorf("retryRequeuesJob(%s) = %v, want %v", test.name, got, test.want)
		}
	}
}
