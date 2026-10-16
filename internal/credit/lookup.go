package credit

import (
	"context"
	"fmt"
	"strings"
)

// LookupRequest returns one request together with its executions and manual
// adjustments, but only when the request belongs to keyID.
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
		req, err := loadRequest(ctx, q, "id = ? AND key_id = ?", requestID, keyID)
		if err != nil {
			return err
		}
		executions, err := lookupExecutions(ctx, q, req.id)
		if err != nil {
			return err
		}
		adjustment, err := counterValue(ctx, q,
			`SELECT COALESCE(SUM(a.amount_micro_usd),0) FROM adjustments a WHERE a.request_id = ?`, req.id)
		if err != nil {
			return fmt.Errorf("summarize adjustments: %w", err)
		}
		out = req.toRequest(executions, adjustment)
		return nil
	})
	if err != nil {
		return Request{}, err
	}
	return out, nil
}

// lookupExecutions loads every execution recorded for one request.
func lookupExecutions(ctx context.Context, q querier, requestID string) ([]Usage, error) {
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
		out = append(out, u.toUsage())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate executions: %w", err)
	}
	return out, nil
}
