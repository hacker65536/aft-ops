// Package account resolves AWS account id/name/email mappings from a
// pluggable source. The AFT management account is usually not the
// Organizations management account, so organizations:ListAccounts may be
// unavailable there — the default source is AFT's own DynamoDB metadata.
package account

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/organizations"

	"github.com/hacker65536/aft-ops/internal/core/model"
)

// Source fetches the full account list from one backend.
type Source interface {
	Name() string
	Fetch(ctx context.Context) ([]model.Account, error)
}

// ---- AFT DynamoDB (default) ----

// DynamoDBAPI is the subset of the DynamoDB client we use.
type DynamoDBAPI interface {
	dynamodb.ScanAPIClient
}

// DynamoSource reads AFT's aft-request-metadata table.
type DynamoSource struct {
	Client DynamoDBAPI
	Table  string
}

func (s *DynamoSource) Name() string { return "aft-dynamodb(" + s.Table + ")" }

// metadataItem is a tolerant mapping of an aft-request-metadata item.
// Schema verified against the live table (D3): the partition key `id` is
// the 12-digit vended account id, with a flat set of scalar attributes;
// there is no `vended_account_id` or nested `account_request`. Unknown
// fields are ignored and missing ones degrade gracefully.
type metadataItem struct {
	ID                 string `dynamodbav:"id"` // partition key = 12-digit account id
	Email              string `dynamodbav:"email"`
	AccountName        string `dynamodbav:"account_name"`
	CustomizationsName string `dynamodbav:"account_customizations_name"`
	AccountStatus      string `dynamodbav:"account_status"`
}

func (s *DynamoSource) Fetch(ctx context.Context) ([]model.Account, error) {
	var accounts []model.Account
	p := dynamodb.NewScanPaginator(s.Client, &dynamodb.ScanInput{TableName: &s.Table})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", s.Table, err)
		}
		for _, raw := range page.Items {
			var item metadataItem
			if err := attributevalue.UnmarshalMap(raw, &item); err != nil {
				continue // tolerate schema drift per item
			}
			acc := item.toAccount()
			if acc.ID != "" {
				accounts = append(accounts, acc)
			}
		}
	}
	return accounts, nil
}

func (i metadataItem) toAccount() model.Account {
	acc := model.Account{
		ID:    i.ID,
		Name:  i.AccountName,
		Email: i.Email,
	}
	if acc.Name == "" {
		acc.Name = i.CustomizationsName
	}
	return acc
}

// ---- Organizations ----

// OrganizationsAPI is the subset of the Organizations client we use.
type OrganizationsAPI interface {
	organizations.ListAccountsAPIClient
}

// OrgSource reads organizations:ListAccounts (management account or
// delegated admin only).
type OrgSource struct {
	Client OrganizationsAPI
}

func (s *OrgSource) Name() string { return "organizations" }

func (s *OrgSource) Fetch(ctx context.Context) ([]model.Account, error) {
	var accounts []model.Account
	p := organizations.NewListAccountsPaginator(s.Client, &organizations.ListAccountsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("organizations ListAccounts: %w", err)
		}
		for _, a := range page.Accounts {
			acc := model.Account{}
			if a.Id != nil {
				acc.ID = *a.Id
			}
			if a.Name != nil {
				acc.Name = *a.Name
			}
			if a.Email != nil {
				acc.Email = *a.Email
			}
			if acc.ID != "" {
				accounts = append(accounts, acc)
			}
		}
	}
	return accounts, nil
}

// ---- Static file ----

// StaticSource reads accounts from a local JSON or CSV file
// (offline/fallback use).
type StaticSource struct {
	Path string
}

func (s *StaticSource) Name() string { return "static(" + s.Path + ")" }

func (s *StaticSource) Fetch(_ context.Context) ([]model.Account, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(filepath.Ext(s.Path)) {
	case ".json":
		var accounts []model.Account
		if err := json.Unmarshal(data, &accounts); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.Path, err)
		}
		return accounts, nil
	case ".csv":
		return parseCSV(data)
	default:
		return nil, fmt.Errorf("unsupported static accounts file %s (want .json or .csv)", s.Path)
	}
}

// parseCSV accepts "id,name[,email]" rows; a header line is skipped when
// the first field is not a 12-digit id.
func parseCSV(data []byte) ([]model.Account, error) {
	rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		return nil, err
	}
	var accounts []model.Account
	for i, row := range rows {
		if len(row) < 2 {
			continue
		}
		id := strings.TrimSpace(row[0])
		if !isAccountID(id) {
			if i == 0 {
				continue // header
			}
			return nil, fmt.Errorf("row %d: invalid account id %q", i+1, id)
		}
		acc := model.Account{ID: id, Name: strings.TrimSpace(row[1])}
		if len(row) > 2 {
			acc.Email = strings.TrimSpace(row[2])
		}
		accounts = append(accounts, acc)
	}
	return accounts, nil
}

func isAccountID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
