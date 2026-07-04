package market

import (
	"context"
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
		return ledger.Market{}, err
	}
	s.tel.Track(participant, "market_created", map[string]any{"kind": kindName, "context": ref})
	return created, nil
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

func (s *Service) Get(ctx context.Context, id int64) (ledger.Market, ledger.USDC, error) {
	return s.led.Get(ctx, id)
}
