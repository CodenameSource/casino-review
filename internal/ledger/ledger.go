package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"casino-review/internal/store"
	"casino-review/internal/telemetry"
)

// Market states. Every transition is a single transaction with a state-guarded
// UPDATE (… WHERE state = …, RowsAffected checked): the double-resolve /
// double-refund defense. Money events are emitted inside the same transaction
// so the experiment record can never drift from the ledger.
const (
	StateOpen     = "OPEN"
	StateLocked   = "LOCKED"
	StateResolved = "RESOLVED"
	StateVoided   = "VOIDED"
)

// Position statuses.
const (
	PosActive   = "ACTIVE"
	PosRefunded = "REFUNDED"
	PosPaid     = "PAID"  // winner: received a payout
	PosSpent    = "SPENT" // stake went to the pool's winners / the solver
)

var (
	ErrNotFound   = errors.New("market not found")
	ErrBadState   = errors.New("market is not in a state that allows this")
	ErrBadOutcome = errors.New("outcome is not one of the market's outcomes")
	ErrNoPosition = errors.New("no active position to refund")
)

// Market is the persisted market row.
type Market struct {
	ID         int64
	Kind       string
	ContextRef string
	Question   string
	Outcomes   []string
	Spec       map[string]any
	State      string
	Resolution map[string]any
	CreatedBy  string
	LocksAt    *time.Time
	ResolvesBy *time.Time
	CreatedAt  time.Time
}

// BoardRow is one entry of the ranked board.
type BoardRow struct {
	Market       Market
	Pool         USDC
	Participants int
}

// Ledger owns all money mutations.
type Ledger struct {
	st *store.Store
}

func New(st *store.Store) *Ledger { return &Ledger{st: st} }

// validOutcomes rejects outcome sets that would corrupt resolution: empties,
// too many, and case-colliding duplicates ("yes" vs "Yes" resolving apart).
func validOutcomes(outcomes []string) error {
	if len(outcomes) == 0 || len(outcomes) > 16 {
		return fmt.Errorf("a market needs 1-16 outcomes, got %d", len(outcomes))
	}
	seen := map[string]bool{}
	for _, o := range outcomes {
		if o == "" || len(o) > 32 {
			return fmt.Errorf("outcome %q must be 1-32 characters", o)
		}
		k := strings.ToLower(o)
		if seen[k] {
			return fmt.Errorf("duplicate outcome %q (case-insensitive)", o)
		}
		seen[k] = true
	}
	return nil
}

// CreateMarket inserts a new OPEN market.
func (l *Ledger) CreateMarket(ctx context.Context, m Market) (Market, error) {
	if err := validOutcomes(m.Outcomes); err != nil {
		return Market{}, err
	}
	outcomes, _ := json.Marshal(m.Outcomes)
	spec, _ := json.Marshal(m.Spec)
	if m.Spec == nil {
		spec = []byte(`{}`)
	}
	tx, err := l.st.Pool.Begin(ctx)
	if err != nil {
		return Market{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	err = tx.QueryRow(ctx,
		`INSERT INTO markets (kind, context_ref, question, outcomes, outcome_spec, created_by, locks_at, resolves_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id, created_at`,
		m.Kind, m.ContextRef, m.Question, outcomes, spec, m.CreatedBy, m.LocksAt, m.ResolvesBy).
		Scan(&m.ID, &m.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Market{}, fmt.Errorf("a live %s market already exists for %s", m.Kind, m.ContextRef)
	}
	if err != nil {
		return Market{}, err
	}
	m.State = StateOpen

	if err := telemetry.Emit(ctx, tx, telemetry.Event{
		Type: "market.created", Actor: m.CreatedBy, ContextRef: m.ContextRef,
		Payload: map[string]any{"market_id": m.ID, "kind": m.Kind, "question": m.Question, "outcomes": m.Outcomes, "via": viaOf(ctx)},
	}); err != nil {
		return Market{}, err
	}
	return m, tx.Commit(ctx)
}

// FindOrCreateBounty returns the live bounty market for a context, creating it
// if needed (concurrency-safe via the partial unique index).
func (l *Ledger) FindOrCreateBounty(ctx context.Context, contextRef, question, createdBy string) (Market, error) {
	if m, err := l.liveBounty(ctx, contextRef); err == nil {
		return m, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Market{}, err
	}
	m, err := l.CreateMarket(ctx, Market{
		Kind: "bounty", ContextRef: contextRef, Question: question,
		Outcomes: []string{"merged"}, CreatedBy: createdBy,
	})
	if err == nil {
		return m, nil
	}
	// Lost a creation race: the winner's market is the market.
	if m2, err2 := l.liveBounty(ctx, contextRef); err2 == nil {
		return m2, nil
	}
	return Market{}, err
}

func (l *Ledger) liveBounty(ctx context.Context, contextRef string) (Market, error) {
	row := l.st.Pool.QueryRow(ctx, marketSelect+
		` WHERE kind='bounty' AND context_ref=$1 AND state IN ('OPEN','LOCKED')`, contextRef)
	return scanMarket(row)
}

// PlacePosition stakes amount on an outcome. Allowed only while OPEN.
func (l *Ledger) PlacePosition(ctx context.Context, marketID int64, participant, outcome string, amount USDC) (int64, error) {
	if amount <= 0 {
		return 0, fmt.Errorf("amount must be positive")
	}
	tx, err := l.st.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// Lock the market row so a concurrent Lock/Resolve can't slip between the
	// state check and the insert.
	m, err := lockMarket(ctx, tx, marketID)
	if err != nil {
		return 0, err
	}
	if m.State != StateOpen {
		return 0, fmt.Errorf("%w: %s is %s", ErrBadState, m.ContextRef, m.State)
	}
	if !slices.Contains(m.Outcomes, outcome) {
		return 0, fmt.Errorf("%w: %q (have: %v)", ErrBadOutcome, outcome, m.Outcomes)
	}

	var posID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO positions (market_id, participant, outcome, amount_usdc) VALUES ($1,$2,$3,$4) RETURNING id`,
		marketID, participant, outcome, int64(amount)).Scan(&posID); err != nil {
		return 0, err
	}
	if err := telemetry.Emit(ctx, tx, telemetry.Event{
		Type: "position.placed", Actor: participant, ContextRef: m.ContextRef,
		Payload: map[string]any{"market_id": marketID, "position_id": posID, "outcome": outcome,
			"amount_usdc": int64(amount), "via": viaOf(ctx)},
	}); err != nil {
		return 0, err
	}
	return posID, tx.Commit(ctx)
}

// Refund returns ALL of a participant's ACTIVE positions on an OPEN market.
func (l *Ledger) Refund(ctx context.Context, marketID int64, participant string) (USDC, error) {
	tx, err := l.st.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	m, err := lockMarket(ctx, tx, marketID)
	if err != nil {
		return 0, err
	}
	if m.State != StateOpen {
		return 0, fmt.Errorf("%w: %s is %s", ErrBadState, m.ContextRef, m.State)
	}
	var total int64
	if err := tx.QueryRow(ctx,
		`WITH refunded AS (
		   UPDATE positions SET status='REFUNDED', updated_at=now()
		   WHERE market_id=$1 AND participant=$2 AND status='ACTIVE'
		   RETURNING amount_usdc)
		 SELECT COALESCE(SUM(amount_usdc),0) FROM refunded`, marketID, participant).Scan(&total); err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, ErrNoPosition
	}
	if err := telemetry.Emit(ctx, tx, telemetry.Event{
		Type: "position.refunded", Actor: participant, ContextRef: m.ContextRef,
		Payload: map[string]any{"market_id": marketID, "amount_usdc": total, "via": viaOf(ctx)},
	}); err != nil {
		return 0, err
	}
	return USDC(total), tx.Commit(ctx)
}

// Lock freezes betting (OPEN → LOCKED) and snapshots the pools — the
// odds-at-lock record that market calibration (Brier scores) is computed from.
func (l *Ledger) Lock(ctx context.Context, marketID int64, actor string) error {
	tx, err := l.st.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	m, err := lockMarket(ctx, tx, marketID)
	if err != nil {
		return err
	}
	if m.State != StateOpen {
		return fmt.Errorf("%w: %s is %s", ErrBadState, m.ContextRef, m.State)
	}
	pools, err := poolsByOutcome(ctx, tx, marketID)
	if err != nil {
		return err
	}
	if err := guardedUpdate(ctx, tx,
		`UPDATE markets SET state='LOCKED', updated_at=now() WHERE id=$1 AND state='OPEN'`, marketID); err != nil {
		return err
	}
	if err := telemetry.Emit(ctx, tx, telemetry.Event{
		Type: "market.locked", Actor: actor, ContextRef: m.ContextRef,
		Payload: map[string]any{"market_id": marketID, "pools": pools, "via": viaOf(ctx)},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Resolve settles a market on an outcome and applies the kind's payout rule.
// payoutRule: "solver" pays the whole pool to `solver`; "parimutuel" splits it
// among winning positions (no winners ⇒ everyone is refunded).
func (l *Ledger) Resolve(ctx context.Context, marketID int64, outcome, payoutRule, solver, actor string, evidence map[string]any) ([]Payout, error) {
	tx, err := l.st.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	m, err := lockMarket(ctx, tx, marketID)
	if err != nil {
		return nil, err
	}
	if m.State != StateOpen && m.State != StateLocked {
		return nil, fmt.Errorf("%w: %s is %s", ErrBadState, m.ContextRef, m.State)
	}
	if !slices.Contains(m.Outcomes, outcome) {
		return nil, fmt.Errorf("%w: %q (have: %v)", ErrBadOutcome, outcome, m.Outcomes)
	}

	stakes, err := activeStakes(ctx, tx, marketID)
	if err != nil {
		return nil, err
	}
	pools, err := poolsByOutcome(ctx, tx, marketID)
	if err != nil {
		return nil, err
	}

	var payouts []Payout
	switch payoutRule {
	case "solver":
		if solver == "" {
			return nil, fmt.Errorf("solver payout requires a solver")
		}
		payouts = computeSolverPayout(stakes, solver)
		if err := setStatuses(ctx, tx, marketID, PosSpent); err != nil {
			return nil, err
		}
	case "parimutuel":
		payouts = computeParimutuelPayouts(stakes, outcome)
		if payouts == nil {
			// Nobody backed the winning outcome: money goes home.
			if err := setStatuses(ctx, tx, marketID, PosRefunded); err != nil {
				return nil, err
			}
			for _, s := range stakes {
				payouts = append(payouts, Payout{Payee: s.Participant, Amount: s.Amount, Reason: "refund"})
			}
		} else {
			if err := setWinLoss(ctx, tx, marketID, outcome); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unknown payout rule %q", payoutRule)
	}

	for _, p := range payouts {
		if p.Reason == "refund" {
			continue // refunds restore positions; they aren't payout rows
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO payouts (market_id, payee, amount_usdc, reason) VALUES ($1,$2,$3,$4)`,
			marketID, p.Payee, int64(p.Amount), p.Reason); err != nil {
			return nil, err
		}
	}

	resolution := map[string]any{"outcome": outcome, "pools": pools, "evidence": evidence}
	if solver != "" {
		resolution["solver"] = solver
	}
	resBytes, _ := json.Marshal(resolution)
	if err := guardedUpdate(ctx, tx,
		`UPDATE markets SET state='RESOLVED', resolution=$2, resolved_at=now(), updated_at=now()
		 WHERE id=$1 AND state IN ('OPEN','LOCKED')`, marketID, resBytes); err != nil {
		return nil, err
	}

	if err := telemetry.Emit(ctx, tx, telemetry.Event{
		Type: "market.resolved", Actor: actor, ContextRef: m.ContextRef,
		Payload: map[string]any{"market_id": marketID, "outcome": outcome, "pools": pools,
			"payouts": payoutSummaries(payouts), "via": viaOf(ctx)},
	}); err != nil {
		return nil, err
	}
	return payouts, tx.Commit(ctx)
}

// Void cancels a market and refunds every ACTIVE position.
func (l *Ledger) Void(ctx context.Context, marketID int64, actor, reason string) ([]Payout, error) {
	tx, err := l.st.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	m, err := lockMarket(ctx, tx, marketID)
	if err != nil {
		return nil, err
	}
	if m.State != StateOpen && m.State != StateLocked {
		return nil, fmt.Errorf("%w: %s is %s", ErrBadState, m.ContextRef, m.State)
	}
	stakes, err := activeStakes(ctx, tx, marketID)
	if err != nil {
		return nil, err
	}
	if err := setStatuses(ctx, tx, marketID, PosRefunded); err != nil {
		return nil, err
	}
	if err := guardedUpdate(ctx, tx,
		`UPDATE markets SET state='VOIDED', updated_at=now() WHERE id=$1 AND state IN ('OPEN','LOCKED')`, marketID); err != nil {
		return nil, err
	}
	var refunds []Payout
	for _, s := range stakes {
		refunds = append(refunds, Payout{Payee: s.Participant, Amount: s.Amount, Reason: "refund"})
	}
	if err := telemetry.Emit(ctx, tx, telemetry.Event{
		Type: "market.voided", Actor: actor, ContextRef: m.ContextRef,
		Payload: map[string]any{"market_id": marketID, "reason": reason,
			"refunds": payoutSummaries(refunds), "via": viaOf(ctx)},
	}); err != nil {
		return nil, err
	}
	return refunds, tx.Commit(ctx)
}

// Board returns live markets ranked by committed pool.
func (l *Ledger) Board(ctx context.Context, limit int) ([]BoardRow, error) {
	rows, err := l.st.Pool.Query(ctx, marketSelect+`
		 WHERE state IN ('OPEN','LOCKED')
		 ORDER BY (SELECT COALESCE(SUM(amount_usdc),0) FROM positions p WHERE p.market_id=markets.id AND p.status='ACTIVE') DESC,
		          created_at ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var board []BoardRow
	for rows.Next() {
		m, err := scanMarket(rows)
		if err != nil {
			return nil, err
		}
		board = append(board, BoardRow{Market: m})
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	for i := range board {
		err := l.st.Pool.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount_usdc),0), COUNT(DISTINCT participant)
			 FROM positions WHERE market_id=$1 AND status='ACTIVE'`, board[i].Market.ID).
			Scan(&board[i].Pool, &board[i].Participants)
		if err != nil {
			return nil, err
		}
	}
	return board, nil
}

// Get fetches one market with its pool.
func (l *Ledger) Get(ctx context.Context, id int64) (Market, USDC, error) {
	m, err := scanMarket(l.st.Pool.QueryRow(ctx, marketSelect+` WHERE id=$1`, id))
	if err != nil {
		return Market{}, 0, err
	}
	var pool int64
	if err := l.st.Pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount_usdc),0) FROM positions WHERE market_id=$1 AND status='ACTIVE'`, id).Scan(&pool); err != nil {
		return Market{}, 0, err
	}
	return m, USDC(pool), nil
}

// --- helpers ---

const marketSelect = `SELECT id, kind, context_ref, question, outcomes, outcome_spec, state, resolution, created_by, locks_at, resolves_by, created_at FROM markets`

type rowScanner interface{ Scan(dest ...any) error }

func scanMarket(row rowScanner) (Market, error) {
	var m Market
	var outcomes, spec []byte
	var resolution []byte
	err := row.Scan(&m.ID, &m.Kind, &m.ContextRef, &m.Question, &outcomes, &spec, &m.State, &resolution,
		&m.CreatedBy, &m.LocksAt, &m.ResolvesBy, &m.CreatedAt)
	if err == pgx.ErrNoRows {
		return Market{}, ErrNotFound
	}
	if err != nil {
		return Market{}, err
	}
	json.Unmarshal(outcomes, &m.Outcomes)
	json.Unmarshal(spec, &m.Spec)
	if resolution != nil {
		json.Unmarshal(resolution, &m.Resolution)
	}
	return m, nil
}

// lockMarket locks the market row for the transaction (SELECT ... FOR UPDATE).
func lockMarket(ctx context.Context, tx pgx.Tx, id int64) (Market, error) {
	return scanMarket(tx.QueryRow(ctx, marketSelect+` WHERE id=$1 FOR UPDATE`, id))
}

// guardedUpdate runs a state-guarded UPDATE and fails loudly if the guard did
// not match — the concurrent-transition tripwire.
func guardedUpdate(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrBadState
	}
	return nil
}

func activeStakes(ctx context.Context, tx pgx.Tx, marketID int64) ([]stake, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, participant, outcome, amount_usdc FROM positions
		 WHERE market_id=$1 AND status='ACTIVE' ORDER BY id FOR UPDATE`, marketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stake
	for rows.Next() {
		var s stake
		var amt int64
		if err := rows.Scan(&s.ID, &s.Participant, &s.Outcome, &amt); err != nil {
			return nil, err
		}
		s.Amount = USDC(amt)
		out = append(out, s)
	}
	return out, rows.Err()
}

func poolsByOutcome(ctx context.Context, tx pgx.Tx, marketID int64) (map[string]int64, error) {
	rows, err := tx.Query(ctx,
		`SELECT outcome, COALESCE(SUM(amount_usdc),0) FROM positions
		 WHERE market_id=$1 AND status='ACTIVE' GROUP BY outcome`, marketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pools := map[string]int64{}
	for rows.Next() {
		var o string
		var v int64
		if err := rows.Scan(&o, &v); err != nil {
			return nil, err
		}
		pools[o] = v
	}
	return pools, rows.Err()
}

func setStatuses(ctx context.Context, tx pgx.Tx, marketID int64, status string) error {
	_, err := tx.Exec(ctx,
		`UPDATE positions SET status=$2, updated_at=now() WHERE market_id=$1 AND status='ACTIVE'`,
		marketID, status)
	return err
}

func setWinLoss(ctx context.Context, tx pgx.Tx, marketID int64, winning string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE positions SET status='PAID', updated_at=now() WHERE market_id=$1 AND status='ACTIVE' AND outcome=$2`,
		marketID, winning); err != nil {
		return err
	}
	_, err := tx.Exec(ctx,
		`UPDATE positions SET status='SPENT', updated_at=now() WHERE market_id=$1 AND status='ACTIVE'`,
		marketID)
	return err
}

func payoutSummaries(ps []Payout) []map[string]any {
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, map[string]any{"payee": p.Payee, "amount_usdc": int64(p.Amount), "reason": p.Reason})
	}
	return out
}

// viaKey tags events with the surface an action came from ("slack" | "cli" |
// "oracle") so the slackbot's notification tailer can skip events it already
// answered in-channel.
type viaKey struct{}

func WithVia(ctx context.Context, via string) context.Context {
	return context.WithValue(ctx, viaKey{}, via)
}

func viaOf(ctx context.Context) string {
	if v, _ := ctx.Value(viaKey{}).(string); v != "" {
		return v
	}
	return "unknown"
}
