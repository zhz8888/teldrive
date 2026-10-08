//go:build integration

package botgateway

import (
	"context"
	"testing"

	"github.com/zhz8888/teldrive/v2/internal/bots"
	testpostgres "github.com/zhz8888/teldrive/v2/internal/testutil/postgres"
)

func TestProvisionBotPromotesEveryChannelPage(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users (user_id) VALUES (1001)"); err != nil {
		t.Fatal(err)
	}
	const channelCount = provisionChannelPageSize + 50
	// One statement gives every channel the same created_at, so the promotion
	// walk has to break ties with the channel_id half of the cursor.
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO channels (channel_id, user_id, name, selected)
SELECT g, 1001, 'channel-' || g, false
FROM generate_series(9000::bigint, 9000 + $1::bigint - 1) AS g`, int64(channelCount)); err != nil {
		t.Fatal(err)
	}
	inviter := &recordingInviter{}
	provisioner, err := NewExistingChannelProvisioner(db.Pool, inviter)
	if err != nil {
		t.Fatal(err)
	}
	if err := provisioner.ProvisionBot(ctx, 1001, bots.Identity{ID: 777, Username: "storage_bot"}); err != nil {
		t.Fatalf("ProvisionBot() error = %v", err)
	}
	if inviter.calls != channelCount {
		t.Fatalf("invited %d channels, want %d", inviter.calls, channelCount)
	}
}

type recordingInviter struct {
	calls int
}

func (r *recordingInviter) InviteBot(context.Context, int64, int64, string) error {
	r.calls++
	return nil
}
