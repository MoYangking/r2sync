package r2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const cloudflareBaseURL = "https://api.cloudflare.com/client/v4"

type CloudflareClient struct {
	token      string
	httpClient *http.Client
	baseURL    string
}

type TokenInfo struct {
	ID string
}

type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func NewCloudflareClient(token string) *CloudflareClient {
	return &CloudflareClient{
		token: strings.TrimSpace(token),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		baseURL: cloudflareBaseURL,
	}
}

func (c *CloudflareClient) VerifyToken(ctx context.Context, accountID string) (TokenInfo, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return TokenInfo{}, errors.New("Cloudflare account id is required for token verification")
	}
	return c.verifyTokenAt(ctx, fmt.Sprintf("/accounts/%s/tokens/verify", accountID))
}

func (c *CloudflareClient) verifyTokenAt(ctx context.Context, path string) (TokenInfo, error) {
	var out struct {
		Success bool `json:"success"`
		Result  struct {
			ID string `json:"id"`
		} `json:"result"`
		Errors []apiError `json:"errors"`
	}
	if err := c.request(ctx, http.MethodGet, path, nil, &out); err != nil {
		return TokenInfo{}, err
	}
	if !out.Success || out.Result.ID == "" {
		return TokenInfo{}, fmt.Errorf("Cloudflare token verification failed: %s", formatAPIErrors(out.Errors))
	}
	return TokenInfo{ID: out.Result.ID}, nil
}

func (c *CloudflareClient) ListAccounts(ctx context.Context) ([]Account, error) {
	var out struct {
		Success bool       `json:"success"`
		Result  []Account  `json:"result"`
		Errors  []apiError `json:"errors"`
	}
	if err := c.request(ctx, http.MethodGet, "/accounts", nil, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, fmt.Errorf("Cloudflare account discovery failed: %s", formatAPIErrors(out.Errors))
	}
	return out.Result, nil
}

func (c *CloudflareClient) CreateBucket(ctx context.Context, accountID, bucketName string) error {
	body := map[string]string{"name": bucketName}
	var out struct {
		Success bool       `json:"success"`
		Errors  []apiError `json:"errors"`
	}
	path := fmt.Sprintf("/accounts/%s/r2/buckets", accountID)
	if err := c.request(ctx, http.MethodPost, path, body, &out); err != nil {
		return err
	}
	if !out.Success {
		msg := formatAPIErrors(out.Errors)
		if strings.Contains(strings.ToLower(msg), "already") {
			return nil
		}
		return fmt.Errorf("create R2 bucket %q: %s", bucketName, msg)
	}
	return nil
}

func DeriveS3Secret(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func DiscoverAccountID(ctx context.Context, cf *CloudflareClient, configured string) (string, error) {
	if strings.TrimSpace(configured) != "" {
		return strings.TrimSpace(configured), nil
	}
	accounts, err := cf.ListAccounts(ctx)
	if err != nil {
		return "", err
	}
	if len(accounts) == 0 {
		return "", errors.New("Cloudflare token can access no accounts; set R2SYNC_ACCOUNT_ID explicitly")
	}
	if len(accounts) > 1 {
		names := make([]string, 0, len(accounts))
		for _, account := range accounts {
			names = append(names, account.Name+"("+account.ID+")")
		}
		return "", fmt.Errorf("Cloudflare token can access multiple accounts; set R2SYNC_ACCOUNT_ID explicitly: %s", strings.Join(names, ", "))
	}
	return accounts[0].ID, nil
}

func (c *CloudflareClient) request(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Cloudflare request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("create Cloudflare request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call Cloudflare API: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return fmt.Errorf("read Cloudflare response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Cloudflare API %s %s failed with %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode Cloudflare response: %w", err)
		}
	}
	return nil
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func formatAPIErrors(errs []apiError) string {
	if len(errs) == 0 {
		return "unknown error"
	}
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		if err.Code != 0 {
			parts = append(parts, fmt.Sprintf("%d: %s", err.Code, err.Message))
		} else {
			parts = append(parts, err.Message)
		}
	}
	return strings.Join(parts, "; ")
}
