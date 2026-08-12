package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"casino-review/internal/telemetry"
)

// debitBalance subtracts amt from a participant's balance within tx. The
// `amount_usdc >= amt` guard makes an overdraw impossible even under concurrency
// (RowsAffected==1 or ErrInsufficientFunds) — no row means balance 0, so it also
// returns ErrInsufficientFunds. Emits balance.debited in the same tx.
func debitBalance(ctx context.Context, tx pgx.Tx, participant string, amt USDC, reason string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE balances SET amount_usdc = amount_usdc - $2, updated_at = now()
		 WHERE participant = $1 AND amount_usdc >= $2`, participant, int64(amt))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrInsufficientFunds
	}
	return telemetry.Emit(ctx, tx, telemetry.Event{
		Type: "balance.debited", Actor: participant,
		Payload: map[string]any{"amount_usdc": int64(amt), "reason": reason, "via": viaOf(ctx)},
	})
}

// creditBalance adds amt to a participant's balance within tx (creating the row
// on first credit). Emits balance.credited. A non-positive amount is a no-op.
func creditBalance(ctx context.Context, tx pgx.Tx, participant string, amt USDC, reason, ref string) error {
	if amt <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO balances (participant, amount_usdc) VALUES ($1,$2)
		 ON CONFLICT (participant) DO UPDATE SET amount_usdc = balances.amount_usdc + $2, updated_at = now()`,
		participant, int64(amt)); err != nil {
		return err
	}
	return telemetry.Emit(ctx, tx, telemetry.Event{
		Type: "balance.credited", Actor: participant,
		Payload: map[string]any{"amount_usdc": int64(amt), "reason": reason, "ref": ref, "via": viaOf(ctx)},
	})
}

// Balance returns a participant's spendable balance (0 if they have no row).
func (l *Ledger) Balance(ctx context.Context, participant string) (USDC, error) {
	var v int64
	err := l.st.Pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT amount_usdc FROM balances WHERE participant=$1), 0)`, participant).Scan(&v)
	return USDC(v), err
}

// Credit adds funds to a participant's balance in its own transaction — the
// deposit path (admin seed today, the on-chain deposit watcher later). Always
// available, independent of the balances-enforcement flag.
func (l *Ledger) Credit(ctx context.Context, participant string, amt USDC, reason string) error {
	if amt <= 0 {
		return fmt.Errorf("amount must be positive")
	}
	tx, err := l.st.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := creditBalance(ctx, tx, participant, amt, reason, ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
