package metrics

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

// throttleCodes are the API error codes treated as throttling.
var throttleCodes = map[string]bool{
	"Throttling":                             true,
	"ThrottlingException":                    true,
	"ThrottledException":                     true,
	"TooManyRequestsException":               true,
	"RequestLimitExceeded":                   true,
	"RequestThrottled":                       true,
	"RequestThrottledException":              true,
	"ProvisionedThroughputExceededException": true, // DynamoDB
}

// IsThrottle reports whether err is an AWS throttling error.
func IsThrottle(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return throttleCodes[ae.ErrorCode()]
	}
	return false
}

// Middleware returns an aws.Config.APIOptions hook that records one Entry
// per API call attempt (retries are separate attempts by design: throttle
// analysis needs the raw attempt rate, not the logical call rate).
func Middleware(rec *Recorder) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Deserialize.Add(middleware.DeserializeMiddlewareFunc(
			"aftOpsMetrics",
			func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (
				middleware.DeserializeOutput, middleware.Metadata, error,
			) {
				start := time.Now()
				out, md, err := next.HandleDeserialize(ctx, in)
				e := Entry{
					Time:       start,
					Service:    awsmiddleware.GetServiceID(ctx),
					Operation:  awsmiddleware.GetOperationName(ctx),
					DurationMs: time.Since(start).Milliseconds(),
					Throttled:  IsThrottle(err),
				}
				if err != nil {
					e.Error = err.Error()
				}
				rec.Record(e)
				return out, md, err
			},
		), middleware.After)
	}
}

func newReader(b []byte) io.Reader { return bytes.NewReader(b) }
