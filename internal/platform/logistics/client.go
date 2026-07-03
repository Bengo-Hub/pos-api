// Package logistics is a thin S2S client for logistics-api's dispatch endpoints. pos-api uses it
// to create a delivery task + assign a rider for POS-NATIVE delivery orders (orders logistics does
// not otherwise own — unlike online orders, which ordering-backend drives). Authenticated by the
// shared INTERNAL_SERVICE_KEY.
package logistics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Client calls logistics-api /api/v1/s2s/dispatch/{tenant}/... endpoints.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient builds a logistics dispatch client. Returns nil-safe methods when baseURL/apiKey are empty.
func NewClient(baseURL, apiKey string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{baseURL: baseURL, apiKey: apiKey, http: &http.Client{Timeout: timeout}}
}

// Enabled reports whether the client is configured to make calls.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" && c.apiKey != "" }

// CreateTaskRequest mirrors logistics-api tasks.CreateTaskRequest (the fields pos-api populates).
type CreateTaskRequest struct {
	ExternalReference string         `json:"external_reference"`
	SourceService     string         `json:"source_service"`
	TaskType          string         `json:"task_type"`
	Priority          int            `json:"priority,omitempty"`
	PickupAddress     string         `json:"pickup_address,omitempty"`
	PickupLat         float64        `json:"pickup_lat,omitempty"`
	PickupLng         float64        `json:"pickup_lng,omitempty"`
	PickupContact     string         `json:"pickup_contact,omitempty"`
	DropoffAddress    string         `json:"dropoff_address,omitempty"`
	DropoffLat        float64        `json:"dropoff_lat,omitempty"`
	DropoffLng        float64        `json:"dropoff_lng,omitempty"`
	DropoffContact    string         `json:"dropoff_contact,omitempty"`
	CustomerName      string         `json:"customer_name,omitempty"`
	CustomerPhone     string         `json:"customer_phone,omitempty"`
	Instructions      string         `json:"instructions,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

// TaskResponse is the subset of the created task pos-api needs (the task id + tracking code).
type TaskResponse struct {
	ID           string `json:"id"`
	TrackingCode string `json:"tracking_code"`
	Status       string `json:"status"`
}

// CreateDeliveryTask creates a delivery task in logistics for a POS-native order.
func (c *Client) CreateDeliveryTask(ctx context.Context, tenantID uuid.UUID, req CreateTaskRequest) (*TaskResponse, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("logistics client not configured")
	}
	if req.SourceService == "" {
		req.SourceService = "pos"
	}
	if req.TaskType == "" {
		req.TaskType = "delivery"
	}
	url := fmt.Sprintf("%s/api/v1/s2s/dispatch/%s/tasks", c.baseURL, tenantID.String())
	var out TaskResponse
	if err := c.post(ctx, url, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AssignTask assigns a fleet member (rider) to a logistics task.
func (c *Client) AssignTask(ctx context.Context, tenantID, taskID uuid.UUID, fleetMemberID string) error {
	if !c.Enabled() {
		return fmt.Errorf("logistics client not configured")
	}
	url := fmt.Sprintf("%s/api/v1/s2s/dispatch/%s/tasks/%s/assign", c.baseURL, tenantID.String(), taskID.String())
	body := map[string]string{"fleet_member_id": fleetMemberID}
	return c.post(ctx, url, body, nil)
}

func (c *Client) post(ctx context.Context, url string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("logistics returned HTTP %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode logistics response: %w", err)
		}
	}
	return nil
}
