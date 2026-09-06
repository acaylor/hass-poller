package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type State struct {
	EntityID   string     `json:"entity_id"`
	State      string     `json:"state"`
	Attributes Attributes `json:"attributes"`
}

type Attributes struct {
	UnitOfMeasurement string `json:"unit_of_measurement"`
}

// HistoryState is a single recorded state change returned by the history API.
type HistoryState struct {
	EntityID    string
	State       string
	Unit        string
	LastChanged time.Time
}

type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewClient(baseURL string, token string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *Client) FetchStates(ctx context.Context) ([]State, error) {
	resp, err := c.get(ctx, "/api/states")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, statusError("/api/states", resp)
	}

	var states []State
	if err := json.NewDecoder(resp.Body).Decode(&states); err != nil {
		return nil, fmt.Errorf("decode states payload: %w", err)
	}

	return states, nil
}

// FetchHistory returns every recorded state change in [start, end] for the
// given entities. At least one entity ID is required.
// Results are bounded by Home Assistant's recorder retention (purge_keep_days).
func (c *Client) FetchHistory(ctx context.Context, start, end time.Time, entityIDs []string) ([]HistoryState, error) {
	if len(entityIDs) == 0 {
		return nil, fmt.Errorf("history requires at least one entity ID")
	}
	q := url.Values{}
	q.Set("end_time", end.UTC().Format(time.RFC3339Nano))
	q.Set("significant_changes_only", "0")
	q.Set("filter_entity_id", strings.Join(entityIDs, ","))

	path := "/api/history/period/" + url.PathEscape(start.UTC().Format(time.RFC3339Nano)) + "?" + q.Encode()
	resp, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, statusError("/api/history/period", resp)
	}

	// The endpoint returns one inner array per entity, ordered chronologically.
	var raw [][]struct {
		EntityID    string      `json:"entity_id"`
		State       string      `json:"state"`
		Attributes  *Attributes `json:"attributes"`
		LastChanged time.Time   `json:"last_changed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode history payload: %w", err)
	}

	var out []HistoryState
	for _, series := range raw {
		// Omitted attributes inherit the last unit; explicit empty attributes
		// clear it when a sensor becomes unitless.
		unit := ""
		entityID := ""
		for _, e := range series {
			if e.EntityID != "" {
				entityID = e.EntityID
			}
			if e.Attributes != nil {
				unit = e.Attributes.UnitOfMeasurement
			}
			out = append(out, HistoryState{
				EntityID:    entityID,
				State:       e.State,
				Unit:        unit,
				LastChanged: e.LastChanged,
			})
		}
	}

	return out, nil
}

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", path, err)
	}
	return resp, nil
}

func statusError(endpoint string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("%s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
}
