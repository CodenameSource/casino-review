package market

import (
	"context"
	"errors"
	"fmt"
	"time"

	"casino-review/internal/config"
	"casino-review/internal/ledger"
	"casino-review/internal/telemetry"
)

// Service is the surface both the Slack bot and the CLI drive. It owns
// context-ref parsing, kind semantics, and delegates money to the ledger.
type Service struct {
	cfg *config.Config
	led *ledger.Ledger
	tel *telemetry.T
}

func NewService(cfg *config.Config, led *ledger.Ledger, tel *telemetry.T) *Service {
	return &Service{cfg: cfg, led: led, tel: tel}
}

// Create makes a new market of a kind. specArgs are kind params (e.g.
// deadline for merge-by), already normalized by the caller's parser.
func (s *Service) Create(ctx context.Context, kindName, ctxInput, participant string, spec map[string]any) (ledger.Market, error) {
	kind, ok := Kinds[kindName]
	if !ok {
		return ledger.Market{}, fmt.Errorf("unknown market kind %q (have: bounty, merge-by, findings-count)", kindName)
	}
	ref, err := ParseContextRef(ctxInput, s.cfg.Owner, s.cfg.Repo)
	if err != nil {
		return ledger.Market{}, err
	}
	// Find-or-create: one live market per (context, kind) — a second `open`/`fund`
	// on the same PR returns the existing market instead of a duplicate, so
	// "#123 merge-by" always names exactly one thing.
	if m, err := s.led.LiveMarket(ctx, ref, kindName); err == nil {
		return m, nil
	} else if !errors.Is(err, ledger.ErrNotFound) {
		return ledger.Market{}, err
	}
	if spec == nil {
		spec = map[string]any{}
	}
	if err := kind.ValidateSpec(spec); err != nil {
		return ledger.Market{}, err
	}
	if kindName == "bounty" {
		return s.led.FindOrCreateBounty(ctx, ref, kind.DefaultQuestion(ref, spec), participant)
	}
	m := ledger.Market{
		Kind: kindName, ContextRef: ref,
		Question: kind.DefaultQuestion(ref, spec),
		Outcomes: kind.Outcomes(spec), Spec: spec,
		CreatedBy: participant,
	}
	if d, ok := spec["deadline"].(string); ok {
		if t, err := time.Parse(time.RFC3339, d); err == nil {
			m.LocksAt = &t
			m.ResolvesBy = &t
		}
	}
	created, err := s.led.CreateMarket(ctx, m)
	if err != nil {
		// Lost a creation race: the winner's live market is the market.
		if m2, err2 := s.led.LiveMarket(ctx, ref, kindName); err2 == nil {
			return m2, nil
		}
		return ledger.Market{}, err
	}
	s.tel.Track(participant, "market_created", map[string]any{"kind": kindName, "context": ref})
	return created, nil
}

// MarketFor resolves a user's context input + kind (e.g. "#123", "merge-by") to
// the single live market, with an actionable error when none exists. This is
// how context-first commands (`bet #123 merge-by …`) find their market.
func (s *Service) MarketFor(ctx context.Context, ctxInput, kind string) (ledger.Market, error) {
	if _, ok := Kinds[kind]; !ok {
		return ledger.Market{}, fmt.Errorf("unknown market kind %q (have: bounty, merge-by, findings-count)", kind)
	}
	ref, err := ParseContextRef(ctxInput, s.cfg.Owner, s.cfg.Repo)
	if err != nil {
		return ledger.Market{}, err
	}
	m, err := s.led.LiveMarket(ctx, ref, kind)
	if errors.Is(err, ledger.ErrNotFound) {
		return ledger.Market{}, fmt.Errorf("no open %s market on %s — start one with `/casino open %s %s`", kind, ref, ctxInput, kind)
	}
	return m, err
}

// PRMarkets returns the normalized context ref and every non-voided market on
// it (with the caller's stakes) — the data behind `/casino show #123`.
func (s *Service) PRMarkets(ctx context.Context, ctxInput, participant string) (string, []Detail, error) {
	ref, err := ParseContextRef(ctxInput, s.cfg.Owner, s.cfg.Repo)
	if err != nil {
		return "", nil, err
	}
	ms, err := s.led.MarketsForContext(ctx, ref)
	if err != nil {
		return "", nil, err
	}
	out := make([]Detail, 0, len(ms))
	for _, m := range ms {
		d, err := s.Detail(ctx, m.ID, participant)
		if err != nil {
			return "", nil, err
		}
		out = append(out, d)
	}
	return ref, out, nil
}

// Fund is bounty sugar: find-or-create the bounty for the context and stake on it.
func (s *Service) Fund(ctx context.Context, ctxInput, participant string, amount ledger.USDC) (ledger.Market, error) {
	m, err := s.Create(ctx, "bounty", ctxInput, participant, nil)
	if err != nil {
		return ledger.Market{}, err
	}
	if _, err := s.led.PlacePosition(ctx, m.ID, participant, "merged", amount); err != nil {
		return ledger.Market{}, err
	}
	s.tel.Track(participant, "position_placed", map[string]any{
		"kind": "bounty", "market_id": m.ID, "amount_usdc": int64(amount)})
	return m, nil
}

// Bet stakes on an outcome of an existing market.
func (s *Service) Bet(ctx context.Context, marketID int64, participant, outcome string, amount ledger.USDC) error {
	if _, err := s.led.PlacePosition(ctx, marketID, participant, outcome, amount); err != nil {
		return err
	}
	s.tel.Track(participant, "position_placed", map[string]any{
		"market_id": marketID, "outcome": outcome, "amount_usdc": int64(amount)})
	return nil
}

func (s *Service) Refund(ctx context.Context, marketID int64, participant string) (ledger.USDC, error) {
	amt, err := s.led.Refund(ctx, marketID, participant)
	if err == nil {
		s.tel.Track(participant, "position_refunded", map[string]any{"market_id": marketID, "amount_usdc": int64(amt)})
	}
	return amt, err
}

func (s *Service) Lock(ctx context.Context, marketID int64, actor string) error {
	return s.led.Lock(ctx, marketID, actor)
}

// Resolve settles a market by kind rule. For bounty, solver must be the PR
// author's identity (payee); parimutuel kinds ignore it.
func (s *Service) Resolve(ctx context.Context, marketID int64, outcome, solver, actor string, evidence map[string]any) ([]ledger.Payout, error) {
	m, _, err := s.led.Get(ctx, marketID)
	if err != nil {
		return nil, err
	}
	kind, ok := Kinds[m.Kind]
	if !ok {
		return nil, fmt.Errorf("market %d has unknown kind %q", marketID, m.Kind)
	}
	payouts, err := s.led.Resolve(ctx, marketID, outcome, kind.PayoutRule, solver, actor, evidence)
	if err == nil {
		s.tel.Track(actor, "market_resolved", map[string]any{"market_id": marketID, "kind": m.Kind, "outcome": outcome})
	}
	return payouts, err
}

func (s *Service) Void(ctx context.Context, marketID int64, actor, reason string) ([]ledger.Payout, error) {
	return s.led.Void(ctx, marketID, actor, reason)
}

func (s *Service) Board(ctx context.Context, limit int) ([]ledger.BoardRow, error) {
	return s.led.Board(ctx, limit)
}

// BoardDetails is the board enriched with per-outcome pools so each row can show
// live odds. No participant stakes (the board is a shared, in-channel view).
func (s *Service) BoardDetails(ctx context.Context, limit int) ([]Detail, error) {
	rows, err := s.led.Board(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Detail, 0, len(rows))
	for _, r := range rows {
		pools, err := s.led.OutcomePools(ctx, r.Market.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, Detail{Market: r.Market, Pool: r.Pool, Backers: r.Participants, OutcomePools: pools})
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, id int64) (ledger.Market, ledger.USDC, error) {
	return s.led.Get(ctx, id)
}

// Detail is everything needed to render one market: the market, its total and
// per-outcome pools, backer count, and the caller's own stake per outcome.
type Detail struct {
	Market       ledger.Market
	Pool         ledger.USDC
	Backers      int
	OutcomePools map[string]ledger.USDC
	MyStake      map[string]ledger.USDC
}

func (s *Service) Detail(ctx context.Context, id int64, participant string) (Detail, error) {
	m, pool, err := s.led.Get(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	pools, err := s.led.OutcomePools(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	backers, err := s.led.ParticipantCount(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	mine, err := s.led.StakeByOutcome(ctx, id, participant)
	if err != nil {
		return Detail{}, err
	}
	return Detail{Market: m, Pool: pool, Backers: backers, OutcomePools: pools, MyStake: mine}, nil
}

// MyPositions lists a participant's active stakes across all markets.
func (s *Service) MyPositions(ctx context.Context, participant string) ([]ledger.PositionView, error) {
	return s.led.ActivePositions(ctx, participant)
}
