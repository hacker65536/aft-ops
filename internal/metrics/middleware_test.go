package metrics

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/middleware"
)

// throttleHTTP answers every request the way STS answers a throttled call.
type throttleHTTP struct{}

func (throttleHTTP) Do(*http.Request) (*http.Response, error) {
	body := `<ErrorResponse><Error><Type>Sender</Type><Code>Throttling</Code>` +
		`<Message>Rate exceeded</Message></Error></ErrorResponse>`
	return &http.Response{
		StatusCode: 400,
		Header:     http.Header{"Content-Type": []string{"text/xml"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// A throttled response is an HTTP 400 that only the operation's deserializer
// turns into an error. The recorder has to sit outside it, or every throttle
// is recorded as a success.
func TestMiddlewareRecordsThrottles(t *testing.T) {
	rec, err := NewRecorder(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	client := sts.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  throttleHTTP{},
		APIOptions:  []func(*middleware.Stack) error{Middleware(rec)},
	})
	if _, err := client.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{},
		func(o *sts.Options) { o.RetryMaxAttempts = 1 }); err == nil {
		t.Fatal("the call should fail")
	}
	path := rec.Path()
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"throttled":true`) {
		t.Errorf("the throttle was not recorded:\n%s", data)
	}
}
