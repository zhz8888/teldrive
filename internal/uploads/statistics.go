package uploads

import (
	"context"
	"fmt"
	"time"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
)

// DailyStatistic is one calendar day of completed upload activity.
type DailyStatistic struct {
	// Date is the day the aggregate grouped by, at midnight UTC: the connection
	// pool pins the session time zone to UTC, so the CURRENT_DATE the query groups
	// by is the UTC day the API contract documents.
	Date time.Time
	// UploadedBytes is the summed plaintext size, in bytes, of the sessions that
	// completed that day.
	UploadedBytes int64
	// CompletedFiles counts the sessions that completed that day.
	CompletedFiles int64
}

// Statistics returns the user's completed upload activity for the last days
// calendar days, oldest first, with one entry per day: days without completions
// come back as zero-valued rows rather than being omitted. days must be between 1
// and 366 inclusive and userID positive, otherwise ErrInvalidInput is returned; a
// nil service is rejected the same way instead of panicking.
func (s *Service) Statistics(ctx context.Context, userID int64, days int32) ([]DailyStatistic, error) {
	if s == nil || s.queries == nil || userID <= 0 || days < 1 || days > 366 {
		return nil, ErrInvalidInput
	}
	rows, err := s.queries.ListUploadDailyStatistics(ctx, sqlcgen.ListUploadDailyStatisticsParams{
		Days: days, UserID: userID,
	})
	if err != nil {
		return nil, fmt.Errorf("query upload statistics: %w", err)
	}
	items := make([]DailyStatistic, 0, len(rows))
	for _, row := range rows {
		if !row.Day.Valid {
			return nil, fmt.Errorf("query upload statistics: invalid day")
		}
		items = append(items, DailyStatistic{
			Date: row.Day.Time, UploadedBytes: row.UploadedBytes, CompletedFiles: row.CompletedFiles,
		})
	}
	return items, nil
}
