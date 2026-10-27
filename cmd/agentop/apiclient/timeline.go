package apiclient

import (
	"context"
	"fmt"
	"net/url"

	"github.com/rossoctl/cortex/core/session/timeline"
)

// GetTimeline fetches the session's spans, whole: the pane bins every window from one fetch.
// ErrNotFound means an unknown session, or a proxy too old to serve the endpoint; the two
// share a 404, and the Timeline pane says which is likelier.
func (c *Client) GetTimeline(ctx context.Context, id string) (*timeline.Timeline, error) {
	var tl timeline.Timeline
	if err := c.getJSON(ctx, fmt.Sprintf("/v1/sessions/%s/timeline", url.PathEscape(id)), &tl); err != nil {
		return nil, err
	}
	return &tl, nil
}
