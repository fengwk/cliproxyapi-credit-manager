package credit

import (
	"context"
	"fmt"
	"strings"
)

// LookupRequest returns one request together with its executions, but only when
// the request belongs to keyID. Each execution is priced with the current price
// book inside the same snapshot, so the customer view agrees with Report and
// Analytics for the same selection.
//
// Ownership is part of the query, never a post-filter: a request that does not
// exist and a request owned by another key are both reported as ErrNotFound, so
// a customer-facing snapshot lookup can never be used as an existence oracle
// across keys. The method is deliberately outside Backend: only the public
// self-service handler needs it, and it is asserted as a narrow optional
// interface there so the shared contract stays unchanged.
func (s *Store) LookupRequest(ctx context.Context, keyID, requestID string) (Request, error) {
	keyID = strings.TrimSpace(keyID)
	requestID = strings.TrimSpace(requestID)
	if keyID == "" || requestID == "" {
		return Request{}, fmt.Errorf("%w: key and request id are required", ErrInvalid)
	}
	var out Request
	err := s.withSnapshot(ctx, func(q querier) error {
		book, err := loadPriceBook(ctx, q)
		if err != nil {
			return err
		}
		req, err := loadRequest(ctx, q, "id = ? AND key_id = ?", requestID, keyID)
		if err != nil {
			return err
		}
		executions, err := lookupExecutions(ctx, q, book, req.id)
		if err != nil {
			return err
		}
		out = req.toRequest(executions)
		return nil
	})
	if err != nil {
		return Request{}, err
	}
	return out, nil
}

// lookupExecutions loads every execution recorded for one request and prices it
// with the caller's price book.
func lookupExecutions(ctx context.Context, q querier, book *priceBook, requestID string) ([]Usage, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+usageColumns+` FROM usages u
		WHERE u.request_id = ? ORDER BY u.at, u.id`, requestID)
	if err != nil {
		return nil, fmt.Errorf("list executions: %w", err)
	}
	defer rows.Close()
	out := []Usage{}
	for rows.Next() {
		u, err := scanUsage(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan execution: %w", err)
		}
		if err := book.applyCost(u); err != nil {
			return nil, err
		}
		out = append(out, u.toUsage())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate executions: %w", err)
	}
	return out, nil
}
