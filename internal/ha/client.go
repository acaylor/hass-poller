package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	StateClass        string `json:"state_class"`
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
	url := c.baseURL + "/api/states"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request /api/states: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("/api/states returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var states []State
	if err := json.NewDecoder(resp.Body).Decode(&states); err != nil {
		return nil, fmt.Errorf("decode states payload: %w", err)
	}

	return states, nil
}
