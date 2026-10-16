package credit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

const (
	// billingToken bills the canonical token buckets. It is the default.
	billingToken = "token"
	// billingPerImage bills a flat price per requested image, ignoring token
	// counters.
	billingPerImage = "per_image"

	// priceTierContext selects a tier by canonical prompt size.
	priceTierContext = "context"
	// priceTierService selects a tier by the request's service tier.
	priceTierService = "service"

	// maxImageCount caps the requested image count that is held and billed.
	maxImageCount = 32
	// maxPriceTiers bounds the tier list of one price.
	maxPriceTiers = 16
)

// resolveBillingMode treats an omitted or unknown token mode as token billing.
func resolveBillingMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), billingPerImage) {
		return billingPerImage
	}
	return billingToken
}

// IsPerImage reports whether the price bills per requested image.
func (p Price) IsPerImage() bool { return resolveBillingMode(p.BillingMode) == billingPerImage }

// imageCountFor resolves the billed image count of a per-image request.
//
// An omitted count means one image. An explicitly declared count must be one of
// the supported 1..32 values: it is refused rather than silently billed as a
// different number, so an unsupported request can never be under- or
// over-charged. Token prices never call this, so an irrelevant image hint on a
// token request is not rejected.
func imageCountFor(in ReserveInput) (int64, error) {
	if !in.ImageCountDeclared {
		return 1, nil
	}
	if in.ImageCount < 1 || in.ImageCount > maxImageCount {
		return 0, fmt.Errorf("%w: requested image count %d is outside the supported 1..%d range",
			ErrInvalid, in.ImageCount, maxImageCount)
	}
	return in.ImageCount, nil
}

// normalizeServiceTier maps upstream service_tier spellings onto the billing
// key used by service price tiers. The common default spellings collapse to the
// empty key, which means "no special service card"; "prio" is an alias of
// "priority"; every other value keeps its identity so an explicit tier can
// still match it.
func normalizeServiceTier(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	value = strings.ReplaceAll(value, " ", "-")
	switch value {
	case "", "default", "auto", "standard", "on-demand", "scale":
		return ""
	case "prio":
		return "priority"
	default:
		return value
	}
}

// serviceTierMatches reports whether the normalized actual tier is named in the
// comma separated configured list.
func serviceTierMatches(configured, actual string) bool {
	actual = normalizeServiceTier(actual)
	if actual == "" {
		return false
	}
	for _, part := range strings.Split(configured, ",") {
		if normalizeServiceTier(part) == actual {
			return true
		}
	}
	return false
}

// normalizeRatesInPlace validates a rate card and copies the reasoning pointer
// so a stored price never aliases caller memory.
func normalizeRatesInPlace(r *Rates) error {
	if r.Input < 0 || r.Output < 0 || r.CacheRead < 0 || r.CacheWrite < 0 {
		return fmt.Errorf("%w: price rates must not be negative", ErrInvalid)
	}
	if r.Reasoning != nil {
		if *r.Reasoning < 0 {
			return fmt.Errorf("%w: price rates must not be negative", ErrInvalid)
		}
		v := *r.Reasoning
		r.Reasoning = &v
	}
	return nil
}

// normalizeTiers canonicalizes an optional tier list: kinds are lowercased,
// a service list is normalized, and every tier is validated. An entirely empty
// entry is treated as absent.
func normalizeTiers(tiers []PriceTier) ([]PriceTier, error) {
	if len(tiers) == 0 {
		return nil, nil
	}
	if len(tiers) > maxPriceTiers {
		return nil, fmt.Errorf("%w: at most %d price tiers", ErrInvalid, maxPriceTiers)
	}
	out := make([]PriceTier, 0, len(tiers))
	for _, tier := range tiers {
		tier.Kind = strings.ToLower(strings.TrimSpace(tier.Kind))
		tier.Label = strings.TrimSpace(tier.Label)
		tier.Service = normalizeServiceTierList(tier.Service)
		if err := normalizeRatesInPlace(&tier.Rates); err != nil {
			return nil, err
		}
		if tier.Kind == "" && tier.Threshold == 0 && tier.Service == "" &&
			tier.Label == "" && tier.Rates == (Rates{}) {
			continue
		}
		switch tier.Kind {
		case priceTierContext:
			if tier.Threshold <= 0 {
				return nil, fmt.Errorf("%w: context price tier threshold must be positive", ErrInvalid)
			}
			if tier.Service != "" {
				return nil, fmt.Errorf("%w: context price tier must not name a service", ErrInvalid)
			}
		case priceTierService:
			if tier.Service == "" {
				return nil, fmt.Errorf("%w: service price tier must name a service", ErrInvalid)
			}
			if tier.Threshold != 0 {
				return nil, fmt.Errorf("%w: service price tier must not carry a threshold", ErrInvalid)
			}
		default:
			return nil, fmt.Errorf("%w: unknown price tier kind %q", ErrInvalid, tier.Kind)
		}
		out = append(out, tier)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// normalizeServiceTierList canonicalizes the comma separated service names of a
// service tier.
func normalizeServiceTierList(value string) string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.ToLower(strings.TrimSpace(part)); part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, ",")
}

func marshalTiers(tiers []PriceTier) (string, error) {
	if len(tiers) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(tiers)
	if err != nil {
		return "", fmt.Errorf("%w: encode price tiers", ErrInvalid)
	}
	return string(raw), nil
}

func unmarshalTiers(raw string) ([]PriceTier, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || raw == "[]" {
		return nil, nil
	}
	var tiers []PriceTier
	if err := json.Unmarshal([]byte(raw), &tiers); err != nil {
		return nil, fmt.Errorf("%w: decode price tiers", ErrInvalid)
	}
	return normalizeTiers(tiers)
}

// computeCost converts canonical token buckets into an integer microUSD cost
// using the base rates. The product is summed exactly and rounded up once,
// never per bucket.
func computeCost(price Price, tokens Tokens) (int64, error) {
	return ratesCost(price.Rates, tokens)
}

// ratesCost is the exact, round-once bucket sum. It is shared by the base price
// and every selected tier.
func ratesCost(rates Rates, tokens Tokens) (int64, error) {
	if rates.Input < 0 || rates.Output < 0 || rates.CacheRead < 0 || rates.CacheWrite < 0 {
		return 0, fmt.Errorf("%w: negative price rate", ErrInvalid)
	}
	reasoning := rates.Output
	if rates.Reasoning != nil {
		if *rates.Reasoning < 0 {
			return 0, fmt.Errorf("%w: negative price rate", ErrInvalid)
		}
		reasoning = *rates.Reasoning
	}
	buckets := [][2]int64{
		{tokens.Input, rates.Input},
		{tokens.CacheRead, rates.CacheRead},
		{tokens.CacheWrite, rates.CacheWrite},
		{tokens.Output, rates.Output},
		{tokens.Reasoning, reasoning},
	}
	total := new(big.Int)
	part := new(big.Int)
	for _, b := range buckets {
		if b[0] == 0 || b[1] == 0 {
			continue
		}
		part.Mul(big.NewInt(b[0]), big.NewInt(b[1]))
		total.Add(total, part)
	}
	total.Add(total, big.NewInt(microPerUnit-1)) // ceiling
	total.Div(total, big.NewInt(microPerUnit))
	if !total.IsInt64() {
		return 0, fmt.Errorf("%w: cost overflows int64", ErrInvalid)
	}
	return total.Int64(), nil
}

// tokenCost prices canonical tokens with an optional tier list.
//
// A matching service tier always applies. Context tiers are only consulted when
// useContext is true, because a reservation estimate is far too coarse to pick
// a context tier and must not silently over- or under-charge one.
func tokenCost(price Price, tiers []PriceTier, tokens Tokens, serviceTier string, useContext bool) (int64, error) {
	rates, err := effectiveRates(price.Rates, tiers, tokens, serviceTier, useContext)
	if err != nil {
		return 0, err
	}
	return ratesCost(rates, tokens)
}

// effectiveRates selects the rate card for a request. A matching service tier
// wins over every context tier; otherwise the highest matching context
// threshold is used; otherwise the base rates.
func effectiveRates(base Rates, tiers []PriceTier, tokens Tokens, serviceTier string, useContext bool) (Rates, error) {
	if rates, ok := serviceTierRates(base, tiers, serviceTier); ok {
		return rates, nil
	}
	if useContext {
		contextTokens, err := contextTokenTotal(tokens)
		if err != nil {
			return Rates{}, err
		}
		if rates, ok := contextTierRates(base, tiers, contextTokens); ok {
			return rates, nil
		}
	}
	return base, nil
}

// contextTokenTotal is the canonical prompt size used to select a context tier:
// uncached input plus cache read and cache write.
func contextTokenTotal(tokens Tokens) (int64, error) {
	total, err := checkedAdd(tokens.Input, tokens.CacheRead)
	if err != nil {
		return 0, err
	}
	return checkedAdd(total, tokens.CacheWrite)
}

func serviceTierRates(base Rates, tiers []PriceTier, serviceTier string) (Rates, bool) {
	for _, tier := range tiers {
		if strings.ToLower(strings.TrimSpace(tier.Kind)) != priceTierService {
			continue
		}
		if serviceTierMatches(tier.Service, serviceTier) {
			return overlayRates(base, tier.Rates), true
		}
	}
	return Rates{}, false
}

func contextTierRates(base Rates, tiers []PriceTier, contextTokens int64) (Rates, bool) {
	var best *PriceTier
	for i := range tiers {
		tier := &tiers[i]
		if strings.ToLower(strings.TrimSpace(tier.Kind)) != priceTierContext {
			continue
		}
		if tier.Threshold <= 0 || contextTokens < tier.Threshold {
			continue
		}
		if best == nil || tier.Threshold > best.Threshold {
			best = tier
		}
	}
	if best == nil {
		return Rates{}, false
	}
	return overlayRates(base, best.Rates), true
}

// overlayRates applies only the non-zero overlay rates over the base card. A
// zero overlay rate inherits the base rate, so a base rate of zero means free
// and is never treated as "missing".
func overlayRates(base, overlay Rates) Rates {
	out := base
	if overlay.Input != 0 {
		out.Input = overlay.Input
	}
	if overlay.Output != 0 {
		out.Output = overlay.Output
	}
	if overlay.Reasoning != nil {
		v := *overlay.Reasoning
		out.Reasoning = &v
	}
	if overlay.CacheRead != 0 {
		out.CacheRead = overlay.CacheRead
	}
	if overlay.CacheWrite != 0 {
		out.CacheWrite = overlay.CacheWrite
	}
	return out
}

// perImageCost prices a frozen image count once, with overflow protection. The
// count is validated, never clamped: an unsupported count is an error so it can
// not be silently billed as a different number.
func perImageCost(price Price, imageCount int64) (int64, error) {
	if price.PerImageMicroUSD < 0 {
		return 0, fmt.Errorf("%w: negative per-image price", ErrInvalid)
	}
	if imageCount < 1 || imageCount > maxImageCount {
		return 0, fmt.Errorf("%w: image count %d is outside the supported 1..%d range",
			ErrInvalid, imageCount, maxImageCount)
	}
	if price.PerImageMicroUSD == 0 {
		return 0, nil
	}
	if price.PerImageMicroUSD > math.MaxInt64/imageCount {
		return 0, fmt.Errorf("%w: per-image cost overflows int64", ErrInvalid)
	}
	return price.PerImageMicroUSD * imageCount, nil
}

// requestCost prices the canonical tokens of one execution with the frozen
// request price and its frozen tiers. Per-image prices are priced from the
// frozen image count elsewhere, so this helper always bills token buckets.
func requestCost(price Price, tiers []PriceTier, tokens Tokens, serviceTier string) (int64, error) {
	return tokenCost(price, tiers, tokens, serviceTier, true)
}

func normalizePrice(p Price) (Price, error) {
	p.Model = strings.TrimSpace(p.Model)
	if p.Model == "" {
		return Price{}, fmt.Errorf("%w: price model is required", ErrInvalid)
	}
	return normalizePriceBody(p)
}

// normalizePriceBody validates and canonicalizes a price that may not carry a
// model, so a pricing rule whose pattern is the identifier can reuse it.
func normalizePriceBody(p Price) (Price, error) {
	if err := normalizeRatesInPlace(&p.Rates); err != nil {
		return Price{}, err
	}
	switch mode := strings.ToLower(strings.TrimSpace(p.BillingMode)); mode {
	case "", billingToken:
		p.BillingMode = mode
	case billingPerImage:
		p.BillingMode = billingPerImage
	default:
		return Price{}, fmt.Errorf("%w: unknown billing mode %q", ErrInvalid, p.BillingMode)
	}
	if p.PerImageMicroUSD < 0 {
		return Price{}, fmt.Errorf("%w: per-image price must not be negative", ErrInvalid)
	}
	if p.BillingMode != billingPerImage && p.PerImageMicroUSD != 0 {
		return Price{}, fmt.Errorf("%w: a per-image price requires per_image billing", ErrInvalid)
	}
	return p, nil
}

// PutPrice stores a price by exact model identifier, bumping its version.
func (s *Store) PutPrice(ctx context.Context, price Price) error {
	p, err := normalizePrice(price)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO prices
			(model, input_rate, output_rate, reasoning_rate, cache_read_rate, cache_write_rate,
			 billing_mode, per_image_micro_usd, version)
			VALUES(?,?,?,?,?,?,?,?,1)
			ON CONFLICT(model) DO UPDATE SET
			  input_rate=excluded.input_rate,
			  output_rate=excluded.output_rate,
			  reasoning_rate=excluded.reasoning_rate,
			  cache_read_rate=excluded.cache_read_rate,
			  cache_write_rate=excluded.cache_write_rate,
			  billing_mode=excluded.billing_mode,
			  per_image_micro_usd=excluded.per_image_micro_usd,
			  version=prices.version+1`,
			p.Model, p.Rates.Input, p.Rates.Output, p.Rates.Reasoning, p.Rates.CacheRead,
			p.Rates.CacheWrite, p.BillingMode, p.PerImageMicroUSD); err != nil {
			return fmt.Errorf("store price: %w", err)
		}
		return insertAudit(ctx, tx, s.nowUTC(), ActionPriceUpdate, "", "", "", "model="+p.Model)
	})
}

const priceColumns = `model, input_rate, output_rate, reasoning_rate, cache_read_rate,
	cache_write_rate, billing_mode, per_image_micro_usd, version`

func (s *Store) ListPrices(ctx context.Context) ([]Price, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+priceColumns+` FROM prices ORDER BY model`)
	if err != nil {
		return nil, fmt.Errorf("list prices: %w", err)
	}
	defer rows.Close()
	var out []Price
	for rows.Next() {
		p, err := scanPrice(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate prices: %w", err)
	}
	return out, nil
}

func scanPrice(scan func(...any) error) (Price, error) {
	var (
		p         Price
		reasoning sql.NullInt64
	)
	if err := scan(&p.Model, &p.Rates.Input, &p.Rates.Output, &reasoning,
		&p.Rates.CacheRead, &p.Rates.CacheWrite, &p.BillingMode, &p.PerImageMicroUSD,
		&p.Version); err != nil {
		return Price{}, fmt.Errorf("scan price: %w", err)
	}
	if reasoning.Valid {
		v := reasoning.Int64
		p.Rates.Reasoning = &v
	}
	return p, nil
}

// lookupPrice loads the exact requested-model price. Absence is reported as
// ErrUnpriced so the caller can fall through to the pricing rules, and never as
// an implicit zero price.
func lookupPrice(ctx context.Context, q querier, model string) (Price, error) {
	row := q.QueryRowContext(ctx, `SELECT `+priceColumns+` FROM prices WHERE model = ?`, model)
	p, err := scanPrice(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Price{}, ErrUnpriced
	}
	if err != nil {
		return Price{}, err
	}
	return p, nil
}

// priceFromRequestRow rebuilds a legacy request price from its stored rate
// columns when the request predates the additive price snapshot.
func priceFromRequestRow(input, output, cacheRead, cacheWrite, version int64, reasoning sql.NullInt64, model string) Price {
	p := Price{
		Model: model,
		Rates: Rates{
			Input:      input,
			Output:     output,
			CacheRead:  cacheRead,
			CacheWrite: cacheWrite,
		},
		Version: version,
	}
	if reasoning.Valid {
		v := reasoning.Int64
		p.Rates.Reasoning = &v
	}
	return p
}
