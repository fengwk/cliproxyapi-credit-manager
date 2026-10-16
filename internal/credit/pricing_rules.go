package credit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// MatchKind identifies how a pricing rule pattern is compared with a requested
// model id.
type MatchKind string

const (
	// MatchExact compares the model id literally with the pattern.
	MatchExact MatchKind = "exact"
	// MatchGlob matches the model id with a shell-style pattern.
	MatchGlob MatchKind = "glob"
	// MatchRegexp matches the model id with an unanchored regular expression.
	MatchRegexp MatchKind = "regexp"

	// matchKindRank orders equally prioritized candidates so an exact match
	// beats a glob match, which beats a regular expression.
	matchKindRankExact  = 0
	matchKindRankGlob   = 1
	matchKindRankRegexp = 2

	// sourceRank* orders equally prioritized candidates of the same match kind:
	// an explicit rule beats the exact price book entry.
	sourceRankRule  = 0
	sourceRankPrice = 1
)

// PricingRule is an operator-defined pricing override for a model id pattern.
//
// Rules are evaluated deterministically: priority descending, then match kind
// (exact before glob before regexp), then rule id ascending. The first matching
// candidate wins, whether or not its rule is enabled: a disabled winning rule
// denies the model instead of silently falling through to a lower-priority
// price. The exact price book is a priority zero exact candidate that a
// higher-priority rule shadows and that a same-priority pattern rule cannot
// shadow.
//
// Rate tiers live only here, next to the price they overlay; a Price value does
// not repeat them, so a rule and its price can never disagree.
type PricingRule struct {
	ID        string      `json:"id"`
	MatchKind MatchKind   `json:"match_kind"`
	Pattern   string      `json:"pattern"`
	Priority  int64       `json:"priority"`
	Enabled   bool        `json:"enabled"`
	Price     Price       `json:"price"`
	Tiers     []PriceTier `json:"tiers,omitempty"`
}

const pricingRuleColumns = `id, match_kind, pattern, priority, enabled, billing_mode,
	per_image_micro_usd, input_rate, output_rate, reasoning_rate, cache_read_rate,
	cache_write_rate, tiers_json`

// normalizePricingRule validates a rule and canonicalizes its price and tiers.
// The price model is optional because the rule pattern identifies the price; it
// defaults to the pattern so every stored rule carries a stable label.
func normalizePricingRule(r PricingRule) (PricingRule, error) {
	r.ID = strings.TrimSpace(r.ID)
	r.Pattern = strings.TrimSpace(r.Pattern)
	if r.ID == "" || r.Pattern == "" {
		return PricingRule{}, fmt.Errorf("%w: pricing rule id and pattern are required", ErrInvalid)
	}
	switch r.MatchKind {
	case MatchExact, "":
		r.MatchKind = MatchExact
	case MatchGlob:
		if _, err := path.Match(r.Pattern, "probe"); err != nil {
			return PricingRule{}, fmt.Errorf("%w: invalid glob pattern", ErrInvalid)
		}
	case MatchRegexp:
		if _, err := regexp.Compile(r.Pattern); err != nil {
			return PricingRule{}, fmt.Errorf("%w: invalid regexp pattern", ErrInvalid)
		}
	default:
		return PricingRule{}, fmt.Errorf("%w: unknown match kind %q", ErrInvalid, r.MatchKind)
	}
	price, err := normalizePriceBody(r.Price)
	if err != nil {
		return PricingRule{}, err
	}
	price.Model = strings.TrimSpace(price.Model)
	if price.Model == "" {
		price.Model = r.Pattern
	}
	r.Price = price
	if r.Tiers, err = normalizeTiers(r.Tiers); err != nil {
		return PricingRule{}, err
	}
	return r, nil
}

func ruleMatches(r PricingRule, model string) (bool, error) {
	switch r.MatchKind {
	case MatchExact, "":
		return model == r.Pattern, nil
	case MatchGlob:
		matched, err := path.Match(r.Pattern, model)
		if err != nil {
			return false, fmt.Errorf("stored pricing rule %s: %w", r.ID, ErrInvalid)
		}
		return matched, nil
	case MatchRegexp:
		matched, err := regexp.MatchString(r.Pattern, model)
		if err != nil {
			return false, fmt.Errorf("stored pricing rule %s: %w", r.ID, ErrInvalid)
		}
		return matched, nil
	default:
		return false, fmt.Errorf("stored pricing rule %s has invalid match kind %q: %w", r.ID, r.MatchKind, ErrInvalid)
	}
}

func scanPricingRule(scan func(...any) error) (PricingRule, error) {
	var (
		r         PricingRule
		enabled   int
		reasoning sql.NullInt64
		tiersJSON string
	)
	if err := scan(&r.ID, &r.MatchKind, &r.Pattern, &r.Priority, &enabled, &r.Price.BillingMode,
		&r.Price.PerImageMicroUSD, &r.Price.Rates.Input, &r.Price.Rates.Output, &reasoning,
		&r.Price.Rates.CacheRead, &r.Price.Rates.CacheWrite, &tiersJSON); err != nil {
		return PricingRule{}, fmt.Errorf("scan pricing rule: %w", err)
	}
	r.Enabled = enabled != 0
	if reasoning.Valid {
		v := reasoning.Int64
		r.Price.Rates.Reasoning = &v
	}
	tiers, err := unmarshalTiers(tiersJSON)
	if err != nil {
		return PricingRule{}, err
	}
	r.Tiers = tiers
	// The rule pattern is the canonical label of the priced model: the table
	// does not duplicate a separate price.model column, so it is rebuilt here
	// rather than read back empty and losing the label.
	r.Price.Model = r.Pattern
	return r, nil
}

// PutPricingRule upserts one pricing rule. It is an administrator operation;
// requests admitted earlier keep the price frozen on their snapshot.
func (s *Store) PutPricingRule(ctx context.Context, rule PricingRule) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := putPricingRule(ctx, tx, rule); err != nil {
			return err
		}
		return insertAudit(ctx, tx, s.nowUTC(), ActionRuleUpdate, "", "", "", "rule_id="+strings.TrimSpace(rule.ID))
	})
}

func putPricingRule(ctx context.Context, q querier, rule PricingRule) error {
	r, err := normalizePricingRule(rule)
	if err != nil {
		return err
	}
	tiersJSON, err := marshalTiers(r.Tiers)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO pricing_rules(`+pricingRuleColumns+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  match_kind=excluded.match_kind,
		  pattern=excluded.pattern,
		  priority=excluded.priority,
		  enabled=excluded.enabled,
		  billing_mode=excluded.billing_mode,
		  per_image_micro_usd=excluded.per_image_micro_usd,
		  input_rate=excluded.input_rate,
		  output_rate=excluded.output_rate,
		  reasoning_rate=excluded.reasoning_rate,
		  cache_read_rate=excluded.cache_read_rate,
		  cache_write_rate=excluded.cache_write_rate,
		  tiers_json=excluded.tiers_json`,
		r.ID, string(r.MatchKind), r.Pattern, r.Priority, boolInt(r.Enabled), r.Price.BillingMode,
		r.Price.PerImageMicroUSD, r.Price.Rates.Input, r.Price.Rates.Output, r.Price.Rates.Reasoning,
		r.Price.Rates.CacheRead, r.Price.Rates.CacheWrite, tiersJSON); err != nil {
		return fmt.Errorf("store pricing rule: %w", err)
	}
	return nil
}

// DeletePricingRule removes one pricing rule by id.
func (s *Store) DeletePricingRule(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: pricing rule id is required", ErrInvalid)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM pricing_rules WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete pricing rule: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete pricing rule: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: pricing rule %s", ErrNotFound, id)
		}
		return insertAudit(ctx, tx, s.nowUTC(), ActionRuleDelete, "", "", "", "rule_id="+id)
	})
}

// ListPricingRules returns every rule in resolution order.
func (s *Store) ListPricingRules(ctx context.Context) ([]PricingRule, error) {
	return listPricingRules(ctx, s.db)
}

func listPricingRules(ctx context.Context, q querier) ([]PricingRule, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+pricingRuleColumns+`
		FROM pricing_rules ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list pricing rules: %w", err)
	}
	defer rows.Close()
	var out []PricingRule
	for rows.Next() {
		r, err := scanPricingRule(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pricing rules: %w", err)
	}
	return out, nil
}

// pricingCandidate is one matchable price with its deterministic ordering key.
type pricingCandidate struct {
	priority   int64
	kindRank   int
	sourceRank int
	id         string
	price      Price
	tiers      []PriceTier
	rule       *PricingRule
}

func matchKindRank(kind MatchKind) int {
	switch kind {
	case MatchExact, "":
		return matchKindRankExact
	case MatchGlob:
		return matchKindRankGlob
	default:
		return matchKindRankRegexp
	}
}

func candidateLess(a, b pricingCandidate) bool {
	if a.priority != b.priority {
		return a.priority > b.priority
	}
	if a.kindRank != b.kindRank {
		return a.kindRank < b.kindRank
	}
	if a.sourceRank != b.sourceRank {
		return a.sourceRank < b.sourceRank
	}
	return a.id < b.id
}

// resolvePricing picks the price and tiers frozen into one reservation. It
// never returns a fabricated zero price: an absent price book entry with no
// matching rule is ErrUnpriced, and a disabled winning rule denies the model.
func resolvePricing(ctx context.Context, q querier, model string) (Price, []PriceTier, string, error) {
	var exact *Price
	switch p, err := lookupPrice(ctx, q, model); {
	case err == nil:
		exact = &p
	case errors.Is(err, ErrUnpriced):
	default:
		return Price{}, nil, "", err
	}
	rules, err := listPricingRules(ctx, q)
	if err != nil {
		return Price{}, nil, "", err
	}
	candidates := make([]pricingCandidate, 0, len(rules)+1)
	if exact != nil {
		candidates = append(candidates, pricingCandidate{
			priority: 0, kindRank: matchKindRankExact, sourceRank: sourceRankPrice, price: *exact,
		})
	}
	for i := range rules {
		rule := rules[i]
		matched, err := ruleMatches(rule, model)
		if err != nil {
			return Price{}, nil, "", err
		}
		if !matched {
			continue
		}
		price := rule.Price
		price.Model = model
		candidates = append(candidates, pricingCandidate{
			priority: rule.Priority, kindRank: matchKindRank(rule.MatchKind), sourceRank: sourceRankRule,
			id: rule.ID, price: price, tiers: rule.Tiers, rule: &rules[i],
		})
	}
	if len(candidates) == 0 {
		return Price{}, nil, "", ErrUnpriced
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidateLess(candidates[i], candidates[j]) })
	winner := candidates[0]
	if winner.rule != nil && !winner.rule.Enabled {
		return Price{}, nil, "", fmt.Errorf("%w: model %s is disabled by pricing rule %s",
			ErrUnpriced, model, winner.rule.ID)
	}
	return winner.price, winner.tiers, winner.id, nil
}

// PriceMatch is the read-only result of a pricing preview: the effective rate
// card and the cost of the supplied prompt size.
type PriceMatch struct {
	Model        string      `json:"model"`
	RuleID       string      `json:"rule_id,omitempty"`
	BillingMode  string      `json:"billing_mode"`
	ServiceTier  string      `json:"service_tier,omitempty"`
	Rates        Rates       `json:"rates"`
	Tiers        []PriceTier `json:"tiers,omitempty"`
	CostMicroUSD int64       `json:"cost_micro_usd"`
	Price        Price       `json:"price"`
}

// PreviewPrice resolves the price for a model and reports the effective rates
// and the cost of inputTokens canonical prompt tokens. It changes nothing.
func (s *Store) PreviewPrice(ctx context.Context, model, serviceTier string, inputTokens int64) (PriceMatch, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return PriceMatch{}, fmt.Errorf("%w: model is required", ErrInvalid)
	}
	if inputTokens < 0 {
		return PriceMatch{}, fmt.Errorf("%w: input tokens must not be negative", ErrInvalid)
	}
	price, tiers, ruleID, err := resolvePricing(ctx, s.db, model)
	if err != nil {
		return PriceMatch{}, err
	}
	match := PriceMatch{
		Model:       model,
		RuleID:      ruleID,
		BillingMode: resolveBillingMode(price.BillingMode),
		ServiceTier: normalizeServiceTier(serviceTier),
		Tiers:       tiers,
		Price:       price,
	}
	if price.IsPerImage() {
		if match.CostMicroUSD, err = perImageCost(price, 1); err != nil {
			return PriceMatch{}, err
		}
		return match, nil
	}
	tokens := Tokens{Input: inputTokens, Total: inputTokens}
	if match.Rates, err = effectiveRates(price.Rates, tiers, tokens, serviceTier, true); err != nil {
		return PriceMatch{}, err
	}
	if match.CostMicroUSD, err = ratesCost(match.Rates, tokens); err != nil {
		return PriceMatch{}, err
	}
	return match, nil
}
