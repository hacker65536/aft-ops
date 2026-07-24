package account

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakeScan returns a single canned page, satisfying dynamodb.ScanAPIClient.
type fakeScan struct{ out *dynamodb.ScanOutput }

func (f fakeScan) Scan(context.Context, *dynamodb.ScanInput,
	...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	return f.out, nil
}

func str(v string) ddbtypes.AttributeValue { return &ddbtypes.AttributeValueMemberS{Value: v} }

// TestDynamoSourceMapsLiveSchema locks in the verified aft-request-metadata
// schema (D3): partition key `id` is the 12-digit account id, with flat
// account_name / email / account_customizations_name attributes.
func TestDynamoSourceMapsLiveSchema(t *testing.T) {
	src := &DynamoSource{
		Table: "aft-request-metadata",
		Client: fakeScan{out: &dynamodb.ScanOutput{
			Items: []map[string]ddbtypes.AttributeValue{
				{
					"id":                          str("943321203864"),
					"email":                       str("admin+bpaas-ai-dev-root@example.com"),
					"account_name":                str("bpaas-ai-dev"),
					"account_customizations_name": str("bpaas-ai-dev"),
					"account_status":              str("ACTIVE"),
				},
				{ // account_name missing → fall back to customizations name
					"id":                          str("111122223333"),
					"account_customizations_name": str("only-custom"),
				},
				{ // no id → must be skipped
					"account_name": str("orphan"),
				},
			},
		}},
	}

	accounts, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want 2 (the id-less item must be dropped)", len(accounts))
	}

	byID := map[string]struct{ name, email string }{}
	for _, a := range accounts {
		byID[a.ID] = struct{ name, email string }{a.Name, a.Email}
	}
	if got := byID["943321203864"]; got.name != "bpaas-ai-dev" || got.email != "admin+bpaas-ai-dev-root@example.com" {
		t.Errorf("943321203864 mapped to %+v, want name=bpaas-ai-dev with email", got)
	}
	if got := byID["111122223333"]; got.name != "only-custom" {
		t.Errorf("name fallback failed: got %q, want only-custom", got.name)
	}
}
