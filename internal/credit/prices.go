package credit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// computeCost converts canonical token buckets into an integer microUSD cost.
// The product is summed exactly and rounded up once, never per bucket.
func computeCost(price Price, tokens Tokens) (int64, error) {
	if price.Rates.Input < 0 || price.Rates.Output < 0 || price.Rates.CacheRead < 0 || price.Rates.CacheWrite < 0 {
		return 0, fmt.Errorf("%w: negative price rate", ErrInvalid)
	}
	reasoning := price.Rates.Output
	if price.Rates.Reasoning != nil {
		if *price.Rates.Reasoning < 0 {
			return 0, fmt.Errorf("%w: negative price rate", ErrInvalid)
		}
		reasoning = *price.Rates.Reasoning
	}
	buckets := [][2]int64{
		{tokens.Input, price.Rates.Input},
		{tokens.CacheRead, price.Rates.CacheRead},
		{tokens.CacheWrite, price.Rates.CacheWrite},
		{tokens.Output, price.Rates.Output},
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

func normalizePrice(p Price) (Price, error) {
	p.Model = strings.TrimSpace(p.Model)
	if p.Model == "" {
		return Price{}, fmt.Errorf("%w: price model is required", ErrInvalid)
	}
	if p.Rates.Input < 0 || p.Rates.Output < 0 || p.Rates.CacheRead < 0 || p.Rates.CacheWrite < 0 {
		return Price{}, fmt.Errorf("%w: price rates must not be negative", ErrInvalid)
	}
	if p.Rates.Reasoning != nil {
		if *p.Rates.Reasoning < 0 {
			return Price{}, fmt.Errorf("%w: price rates must not be negative", ErrInvalid)
		}
		v := *p.Rates.Reasoning
		p.Rates.Reasoning = &v
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
			(model, input_rate, output_rate, reasoning_rate, cache_read_rate, cache_write_rate, version)
			VALUES(?,?,?,?,?,?,1)
			ON CONFLICT(model) DO UPDATE SET
			  input_rate=excluded.input_rate,
			  output_rate=excluded.output_rate,
			  reasoning_rate=excluded.reasoning_rate,
			  cache_read_rate=excluded.cache_read_rate,
			  cache_write_rate=excluded.cache_write_rate,
			  version=prices.version+1`,
			p.Model, p.Rates.Input, p.Rates.Output, p.Rates.Reasoning, p.Rates.CacheRead, p.Rates.CacheWrite); err != nil {
			return fmt.Errorf("store price: %w", err)
		}
		return nil
	})
}

func (s *Store) ListPrices(ctx context.Context) ([]Price, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model, input_rate, output_rate, reasoning_rate,
		cache_read_rate, cache_write_rate, version FROM prices ORDER BY model`)
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
		&p.Rates.CacheRead, &p.Rates.CacheWrite, &p.Version); err != nil {
		return Price{}, fmt.Errorf("scan price: %w", err)
	}
	if reasoning.Valid {
		v := reasoning.Int64
		p.Rates.Reasoning = &v
	}
	return p, nil
}

// lookupPrice loads the exact requested-model price. Absence is a denial, never
// an implicit zero price.
func lookupPrice(ctx context.Context, q querier, model string) (Price, error) {
	row := q.QueryRowContext(ctx, `SELECT model, input_rate, output_rate, reasoning_rate,
		cache_read_rate, cache_write_rate, version FROM prices WHERE model = ?`, model)
	p, err := scanPrice(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Price{}, ErrUnpriced
	}
	if err != nil {
		return Price{}, err
	}
	return p, nil
}

// snapshotPrice copies the price into the request columns for later billing.
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
