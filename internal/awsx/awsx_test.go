package awsx

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/middleware"
)

// stubHTTP answers every request with a GetCallerIdentity response and
// counts how many reached the wire.
type stubHTTP struct{ sent int }

func (s *stubHTTP) Do(*http.Request) (*http.Response, error) {
	s.sent++
	body := `<GetCallerIdentityResponse><GetCallerIdentityResult>` +
		`<Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/xml"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// oneToken grants each service a single token that does not refill within a
// test.
func oneToken(string) float64 { return 1.0 / 3600 }

// Every call made through a client carrying RateLimit waits for a token from
// its service's bucket — two clients built from one Limits draw from one
// budget.
func TestRateLimitSharesOneBudget(t *testing.T) {
	limits := &Limits{RateFor: oneToken}
	httpc := &stubHTTP{}
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  httpc,
		APIOptions:  []func(*middleware.Stack) error{RateLimit(limits)},
	}
	a, b := sts.NewFromConfig(cfg), sts.NewFromConfig(cfg)

	if _, err := a.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// The bucket is empty: the other client's call must wait, and with a
	// short deadline it gives up instead of reaching the wire.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := b.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{},
		func(o *sts.Options) { o.RetryMaxAttempts = 1 }); err == nil {
		t.Fatal("second call should have been held back by the shared limiter")
	}
	if httpc.sent != 1 {
		t.Errorf("requests sent = %d, want 1", httpc.sent)
	}
}

// Each service has its own bucket: spending CodePipeline's token leaves
// CloudWatch Logs' untouched, and a service rated 0 is not limited at all.
func TestLimitsArePerService(t *testing.T) {
	limits := &Limits{RateFor: func(svc string) float64 {
		if svc == "STS" {
			return 0
		}
		return oneToken(svc)
	}}
	ctx := context.Background()
	if err := limits.Wait(ctx, "CodePipeline"); err != nil {
		t.Fatalf("first CodePipeline call: %v", err)
	}
	if err := limits.Wait(ctx, "CloudWatch Logs"); err != nil {
		t.Errorf("CloudWatch Logs should not wait on CodePipeline's bucket: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := limits.Wait(short, "CodePipeline"); err == nil {
		t.Error("a second CodePipeline call should wait for the spent bucket")
	}
	for i := 0; i < 3; i++ {
		if err := limits.Wait(short, "STS"); err != nil {
			t.Errorf("a service rated 0 should be unlimited: %v", err)
		}
	}
}
