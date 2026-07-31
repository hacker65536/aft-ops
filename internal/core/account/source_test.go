package account

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/hacker65536/aft-ops/internal/core/model"
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

	byID := map[string]model.Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	if got := byID["943321203864"]; got.Name != "bpaas-ai-dev" || got.Email != "admin+bpaas-ai-dev-root@example.com" {
		t.Errorf("943321203864 mapped to %+v, want name=bpaas-ai-dev with email", got)
	}
	if got := byID["111122223333"]; got.Name != "only-custom" {
		t.Errorf("name fallback failed: got %q, want only-custom", got.Name)
	}
	// The customizations name is carried, not just used as a name fallback:
	// it is what the expected pipeline trigger's file path is derived from.
	for id, want := range map[string]string{
		"943321203864": "bpaas-ai-dev",
		"111122223333": "only-custom",
	} {
		if got := byID[id].CustomizationsName; got != want {
			t.Errorf("%s customizations name = %q, want %q", id, got, want)
		}
	}
}

// An account source that has no notion of a customizations name leaves it
// empty, which is what makes the trigger report say "unknown" rather than
// judging the pipeline against a made-up path.
func TestResolverHasCustomizationsNames(t *testing.T) {
	with := newResolver([]model.Account{
		{ID: "111111111111", Name: "a"},
		{ID: "222222222222", Name: "b", CustomizationsName: "b"},
	}, time.Time{}, false, "test")
	if !with.HasCustomizationsNames() {
		t.Error("one account with a customizations name should report true")
	}
	without := newResolver([]model.Account{{ID: "111111111111", Name: "a"}},
		time.Time{}, false, "test")
	if without.HasCustomizationsNames() {
		t.Error("no account with a customizations name should report false")
	}
}
