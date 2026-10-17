package credit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
)

const (
	// billingToken prices the canonical token buckets. It is the default.
	billingToken = "token"
	// billingPerImage quotes a flat per-image rate for price previews only:
	// recorded usage has no actual image count and cannot use this mode.
	billingPerImage = "per_image"

	// priceTierContext selects a tier by canonical prompt size.
	priceTierContext = "context"
	// priceTierService selects a tier by the request's service tier.
	priceTierService = "service"

	// maxImageCount bounds the hypothetical image count accepted by a preview.
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

// IsPerImage reports whether this is a per-image reference price.
func (p Price) IsPerImage() bool { return resolveBillingMode(p.BillingMode) == billingPerImage }

// normalizeServiceTier maps upstream service_tier spellings onto the canonical
// tier key used by service price tiers. The common default spellings collapse
// to the empty key, which means "no special service card"; "prio" is an alias of
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

// tokenCost prices canonical tokens with an optional tier list. A matching
// service tier wins; otherwise the highest matching context tier applies.
func tokenCost(price Price, tiers []PriceTier, tokens Tokens, serviceTier string) (int64, error) {
	rates, err := effectiveRates(price.Rates, tiers, tokens, serviceTier)
	if err != nil {
		return 0, err
	}
	return ratesCost(rates, tokens)
}

// effectiveRates selects the rate card for a request. A matching service tier
// wins over every context tier; otherwise the highest matching context
// threshold is used; otherwise the base rates.
func effectiveRates(base Rates, tiers []PriceTier, tokens Tokens, serviceTier string) (Rates, error) {
	if rates, ok := serviceTierRates(base, tiers, serviceTier); ok {
		return rates, nil
	}
	contextTokens, err := contextTokenTotal(tokens)
	if err != nil {
		return Rates{}, err
	}
	if rates, ok := contextTierRates(base, tiers, contextTokens); ok {
		return rates, nil
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

// perImageCost computes a hypothetical image-price preview with overflow
// protection. It is never used to price execution logs.
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

// normalizePrice is the write-path validation of one price book entry.
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

// priceBook is the pricing view of one query: the exact price table and the
// operator rules, read once from a single snapshot and then consulted per
// actual model. Matching is cached per model, so a query never issues one price
// lookup per execution, and resolution never consults a durable snapshot.
//
// A disabled rule is skipped: it simply does not participate in pricing, which
// is why a disabled rule can never deny a model or gate a request.
type priceBook struct {
	exact   map[string]Price
	rules   []PricingRule
	matches map[string]priceResolution
}

// priceResolution is the effective price of one actual model: the winning rate
// card, its tiers and whether any candidate matched at all. An unmatched model
// is unpriced, never free.
type priceResolution struct {
	price  Price
	tiers  []PriceTier
	ruleID string
	priced bool
}

// loadPriceBook reads every price and rule in one pass, without one query per
// model. Both halves come from the caller's snapshot, so a query that prices
// history sees one consistent price book.
func loadPriceBook(ctx context.Context, q querier) (*priceBook, error) {
	book := &priceBook{exact: map[string]Price{}, matches: map[string]priceResolution{}}
	rows, err := q.QueryContext(ctx, `SELECT `+priceColumns+` FROM prices`)
	if err != nil {
		return nil, fmt.Errorf("load price book: %w", err)
	}
	for rows.Next() {
		p, err := scanPrice(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		book.exact[p.Model] = p
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate price book: %w", err)
	}
	rows.Close()
	rules, err := listPricingRules(ctx, q)
	if err != nil {
		return nil, err
	}
	book.rules = rules
	return book, nil
}

// resolve returns the effective price of one actual model id, caching the
// outcome so repeated executions of the same model share one match. The model
// is compared verbatim: an execution model is the price key, and a request
// alias never stands in for it.
func (b *priceBook) resolve(model string) (priceResolution, error) {
	if hit, ok := b.matches[model]; ok {
		return hit, nil
	}
	candidates := make([]pricingCandidate, 0, len(b.rules)+1)
	if exact, ok := b.exact[model]; ok {
		candidates = append(candidates, pricingCandidate{
			priority: 0, kindRank: matchKindRankExact, sourceRank: sourceRankPrice, price: exact,
		})
	}
	for i := range b.rules {
		rule := b.rules[i]
		// A disabled rule takes no part in pricing: skipping it lets the next
		// candidate win instead of turning the rule into a permission gate.
		if !rule.Enabled {
			continue
		}
		matched, err := ruleMatches(rule, model)
		if err != nil {
			return priceResolution{}, err
		}
		if !matched {
			continue
		}
		price := rule.Price
		price.Model = model
		candidates = append(candidates, pricingCandidate{
			priority: rule.Priority, kindRank: matchKindRank(rule.MatchKind), sourceRank: sourceRankRule,
			id: rule.ID, price: price, tiers: rule.Tiers,
		})
	}
	var out priceResolution
	if len(candidates) > 0 {
		sort.SliceStable(candidates, func(i, j int) bool { return candidateLess(candidates[i], candidates[j]) })
		winner := candidates[0]
		out = priceResolution{price: winner.price, tiers: winner.tiers, ruleID: winner.id, priced: true}
	}
	b.matches[model] = out
	return out, nil
}

// executionCost prices one recorded execution with the current book.
//
// It returns the cost and whether the execution is priced at all. An execution
// is unpriced when its usage is not trustworthy, when its actual model has no
// price, when the winning price bills per image (the host reports no real image
// count, so a per-image amount cannot be proven from the log), or when the
// product cannot be represented as an exact int64. Unpriced is reported as
// "not priced" rather than as an error or as a fabricated zero: a wild price
// must never fail a whole report, and it must never become a silent amount.
func (b *priceBook) executionCost(model string, tokens Tokens, serviceTier string, reliable bool) (int64, bool, error) {
	if !reliable {
		return 0, false, nil
	}
	res, err := b.resolve(model)
	if err != nil {
		return 0, false, err
	}
	if !res.priced || res.price.IsPerImage() {
		return 0, false, nil
	}
	cost, err := tokenCost(res.price, res.tiers, tokens, serviceTier)
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return cost, true, nil
}
