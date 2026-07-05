// Package erp is a thin S2S client for erp-api, used to push a POS staff purchase (goods taken on
// credit/layaway funded from salary) into an ERP payroll recoverable. Keyed on the shared auth user
// id; erp resolves/creates the employee. Uses the shared INTERNAL_SERVICE_KEY via X-API-Key +
// X-Tenant-ID (erp resolves the tenant from the header for S2S). See feedback_s2s_service_key.
package erp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// Client is a thin S2S client for erp-api staff-purchase operations.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	log        *zap.Logger
}

// NewClient builds the erp client. baseURL = ERP_SERVICE_URL; apiKey = shared INTERNAL_SERVICE_KEY.
func NewClient(baseURL, apiKey string, timeout time.Duration, log *zap.Logger) *Client {
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	return &Client{baseURL: baseURL, apiKey: apiKey, httpClient: &http.Client{Timeout: timeout}, log: log.Named("erp-client")}
}

// Enabled reports whether the client is configured (ERP_SERVICE_URL + key set).
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" && c.apiKey != "" }

// StaffPurchaseRequest is the S2S body to create an ERP recoverable for a staff purchase.
type StaffPurchaseRequest struct {
	AuthUserID        string `json:"auth_user_id"`
	Origin            string `json:"origin"` // layaway | credit_sale
	POSReference      string `json:"pos_reference"`
	SourceKey         string `json:"source_key"`
	Principal         string `json:"principal"`
	InstallmentAmount string `json:"installment_amount,omitempty"`
	InstallmentsTotal int    `json:"installments_total,omitempty"`
	EmployeeEmail     string `json:"employee_email,omitempty"`
	EmployeeName      string `json:"employee_name,omitempty"`
}

// StaffPurchaseResponse is erp-api's reply.
type StaffPurchaseResponse struct {
	ID          string `json:"id"`
	EmployeeID  string `json:"employee_id"`
	Outstanding string `json:"outstanding"`
	Status      string `json:"status"`
}

// ErrNotEntitled is returned when erp reports the tenant lacks the premium feature (403).
var ErrNotEntitled = fmt.Errorf("erp: staff_fund_from_salary not included in the tenant's plan")

// CreateStaffPurchase pushes a staff purchase to erp-api. Idempotent on source_key (erp returns the
// existing recoverable on replay). tenantID is sent via X-Tenant-ID for the S2S tenant resolution.
func (c *Client) CreateStaffPurchase(ctx context.Context, tenantID string, req StaffPurchaseRequest) (*StaffPurchaseResponse, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("erp: client not configured (ERP_SERVICE_URL unset)")
	}
	endpoint := fmt.Sprintf("%s/api/v1/hrm/staff-purchases", c.baseURL)
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("erp: marshal staff-purchase: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("erp: build staff-purchase request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", c.apiKey)
	httpReq.Header.Set("X-Tenant-ID", tenantID)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("erp: staff-purchase request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrNotEntitled
	}
	if resp.StatusCode >= 400 {
		c.log.Warn("erp: staff-purchase upstream error", zap.Int("status", resp.StatusCode), zap.String("body", string(body)))
		return nil, fmt.Errorf("erp: upstream error %d: %s", resp.StatusCode, string(body))
	}
	var out StaffPurchaseResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("erp: decode staff-purchase response: %w", err)
	}
	return &out, nil
}
