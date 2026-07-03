package r2

import (
	"context"
	"fmt"
	"strings"

	"r2sync/internal/config"
)

type SetupResult struct {
	AccountID string
	TokenID   string
	Store     *S3Store
}

func Setup(ctx context.Context, cfg config.Config) (SetupResult, error) {
	if err := cfg.ValidateCloudflareConfig(); err != nil {
		return SetupResult{}, err
	}
	cf := NewCloudflareClient(cfg.CloudflareToken)
	accountID, err := DiscoverAccountID(ctx, cf, cfg.AccountID)
	if err != nil {
		return SetupResult{}, err
	}
	tokenInfo, err := cf.VerifyToken(ctx, accountID)
	if err != nil {
		return SetupResult{}, fmt.Errorf("verify Cloudflare token: %w", err)
	}
	secret := DeriveS3Secret(cfg.CloudflareToken)
	store := NewS3Store(accountID, cfg.BucketName, tokenInfo.ID, secret)
	if err := store.HeadBucket(ctx); err != nil {
		if createErr := cf.CreateBucket(ctx, accountID, cfg.BucketName); createErr != nil {
			return SetupResult{}, fmt.Errorf("bucket %q is not accessible and create failed: head error: %v; create error: %w", cfg.BucketName, err, createErr)
		}
		if err := store.HeadBucket(ctx); err != nil {
			return SetupResult{}, fmt.Errorf("bucket %q was created but is not accessible yet: %w", cfg.BucketName, err)
		}
	}
	if strings.TrimSpace(tokenInfo.ID) == "" {
		return SetupResult{}, fmt.Errorf("Cloudflare token verification returned empty token id")
	}
	return SetupResult{AccountID: accountID, TokenID: tokenInfo.ID, Store: store}, nil
}
