package ledger

import "context"

// MarketsForContext returns every non-voided market attached to a context ref
// (a PR or ext: key), newest first — the data behind a PR's market dashboard.
func (l *Ledger) MarketsForContext(ctx context.Context, contextRef string) ([]Market, error) {
	rows, err := l.st.Pool.Query(ctx, marketSelect+
		` WHERE context_ref=$1 AND state <> 'VOIDED' ORDER BY created_at DESC`, contextRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Market
	for rows.Next() {
		m, err := scanMarket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// OutcomePools returns the active pool staked on each outcome of a market.
func (l *Ledger) OutcomePools(ctx context.Context, marketID int64) (map[string]USDC, error) {
	rows, err := l.st.Pool.Query(ctx,
		`SELECT outcome, COALESCE(SUM(amount_usdc),0) FROM positions
		 WHERE market_id=$1 AND status='ACTIVE' GROUP BY outcome`, marketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]USDC{}
	for rows.Next() {
		var o string
		var v int64
		if err := rows.Scan(&o, &v); err != nil {
			return nil, err
		}
		out[o] = USDC(v)
	}
	return out, rows.Err()
}

// StakeByOutcome returns a participant's own active stake on each outcome of a
// market (empty if they have none).
func (l *Ledger) StakeByOutcome(ctx context.Context, marketID int64, participant string) (map[string]USDC, error) {
	rows, err := l.st.Pool.Query(ctx,
		`SELECT outcome, COALESCE(SUM(amount_usdc),0) FROM positions
		 WHERE market_id=$1 AND participant=$2 AND status='ACTIVE' GROUP BY outcome`, marketID, participant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]USDC{}
	for rows.Next() {
		var o string
		var v int64
		if err := rows.Scan(&o, &v); err != nil {
			return nil, err
		}
		out[o] = USDC(v)
	}
	return out, rows.Err()
}

// ParticipantCount is how many distinct participants have an active stake.
func (l *Ledger) ParticipantCount(ctx context.Context, marketID int64) (int, error) {
	var n int
	err := l.st.Pool.QueryRow(ctx,
		`SELECT COUNT(DISTINCT participant) FROM positions WHERE market_id=$1 AND status='ACTIVE'`, marketID).Scan(&n)
	return n, err
}

// PositionView is one of a participant's active stakes, with market context.
type PositionView struct {
	MarketID    int64
	Kind        string
	ContextRef  string
	MarketState string
	Outcome     string
	Amount      USDC
}

// ActivePositions returns a participant's active stakes across all markets,
// aggregated per (market, outcome), most recent market first.
func (l *Ledger) ActivePositions(ctx context.Context, participant string) ([]PositionView, error) {
	rows, err := l.st.Pool.Query(ctx,
		`SELECT p.market_id, m.kind, m.context_ref, m.state, p.outcome, SUM(p.amount_usdc)
		 FROM positions p JOIN markets m ON m.id=p.market_id
		 WHERE p.participant=$1 AND p.status='ACTIVE'
		 GROUP BY p.market_id, m.kind, m.context_ref, m.state, p.outcome
		 ORDER BY p.market_id DESC, p.outcome`, participant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PositionView
	for rows.Next() {
		var v PositionView
		var amt int64
		if err := rows.Scan(&v.MarketID, &v.Kind, &v.ContextRef, &v.MarketState, &v.Outcome, &amt); err != nil {
			return nil, err
		}
		v.Amount = USDC(amt)
		out = append(out, v)
	}
	return out, rows.Err()
}
