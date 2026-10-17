package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// pager walks an offset-paginated list for --all.
type pager struct {
	params *Pagination
	size   int
	offset int
	// total is the most items to return, 0 for no cap.
	total int
}

// newPager reads --limit and --offset. The page size is --limit clamped to the
// spec maximum, or the maximum; --limit is also the cap on items in total.
func newPager(p *Pagination, query []boundOption) *pager {
	pg := &pager{params: p, size: p.MaxLimit}
	for _, o := range query {
		if !o.changed() {
			continue
		}
		n, _ := strconv.Atoi(o.value.raw)
		switch o.Field {
		case p.Limit:
			if n > 0 {
				pg.total = n
				pg.size = min(n, p.MaxLimit)
			}
		case p.Offset:
			pg.offset = n
		}
	}
	return pg
}

// setQuery points req at the current page.
func (pg *pager) setQuery(req *request) {
	query := req.url.Query()
	query.Set(pg.params.Offset, strconv.Itoa(pg.offset))
	query.Set(pg.params.Limit, strconv.Itoa(pg.size))
	req.url.RawQuery = query.Encode()
}

// walk moves the offset by the number of items each page returned, and stops at
// the first short or empty page, or when the total is written. The page size is
// never above the spec maximum, so every page but the last is full.
//
// With ndjson each page is written as it arrives. With json the items are
// written as one array only when the walk succeeds, so a failed page never leaves
// stdout holding a cut array that a script could parse.
func (pg *pager) walk(ctx context.Context, t transport, req *request, format outputFormat, w io.Writer) error {
	collected := []json.RawMessage{}
	written := 0
	for {
		pg.setQuery(req)
		body, err := t.send(ctx, req)
		if err != nil {
			return err
		}
		items, err := pageItems(body, pg.params.Results)
		if err != nil {
			return err
		}
		fetched := len(items)
		if pg.total > 0 && written+fetched > pg.total {
			items = items[:pg.total-written]
		}
		if format == outputNDJSON {
			if err := writeLines(w, items); err != nil {
				return err
			}
		} else {
			collected = append(collected, items...)
		}
		written += len(items)
		if fetched < pg.size || (pg.total > 0 && written >= pg.total) {
			break
		}
		pg.offset += fetched
	}
	if format == outputNDJSON {
		return nil
	}
	return encodeJSON(w, collected, "  ")
}

func pageItems(body []byte, field string) ([]json.RawMessage, error) {
	var list map[string]json.RawMessage
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse the list response: %w", err)
	}
	var items []json.RawMessage
	if raw, ok := list[field]; ok {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("parse %q in the list response: %w", field, err)
		}
	}
	return items, nil
}
