package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrLoginTaken means another Slack user already claimed that GitHub login.
var ErrLoginTaken = errors.New("github login already linked to another slack user")

// LinkIdentity maps a Slack user to a GitHub login (payout routing + joining
// behavior across surfaces in the experiment data). A login already claimed
// by a DIFFERENT Slack user is rejected — first-claim wins; otherwise anyone
// could hijack a solver's payout identity right before a resolution. Disputed
// claims are an admin problem (fix the row in SQL), not a race to the bot.
func (s *Store) LinkIdentity(ctx context.Context, slackUserID, githubLogin string) error {
	var holder string
	err := s.Pool.QueryRow(ctx,
		`SELECT slack_user_id FROM identities WHERE github_login=$1 AND slack_user_id<>$2 LIMIT 1`,
		githubLogin, slackUserID).Scan(&holder)
	if err == nil {
		return fmt.Errorf("%w (%s)", ErrLoginTaken, holder)
	}
	if err != pgx.ErrNoRows {
		return err
	}
	_, err = s.Pool.Exec(ctx,
		`INSERT INTO identities (slack_user_id, github_login) VALUES ($1,$2)
		 ON CONFLICT (slack_user_id) DO UPDATE SET github_login=EXCLUDED.github_login, updated_at=now()`,
		slackUserID, githubLogin)
	return err
}

// GithubLogin returns the linked GitHub login for a Slack user ("" if unlinked).
func (s *Store) GithubLogin(ctx context.Context, slackUserID string) (string, error) {
	var login string
	err := s.Pool.QueryRow(ctx,
		`SELECT github_login FROM identities WHERE slack_user_id=$1`, slackUserID).Scan(&login)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return login, err
}
