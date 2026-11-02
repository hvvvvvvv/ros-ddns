package alidns

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	alidnssdk "github.com/alibabacloud-go/alidns-20150109/v5/client"
	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	"github.com/alibabacloud-go/tea/dara"
	"github.com/alibabacloud-go/tea/tea"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
)

type sdkAPI struct {
	client        *alidnssdk.Client
	httpClient    *http.Client
	transportOnce sync.Once
	idleTransport atomic.Pointer[http.Transport]
}

var _ api = (*sdkAPI)(nil)

func newSDKAPI(cfg config.ProviderConfig) (*sdkAPI, error) {
	// The SDK uses whole milliseconds. Keep positive sub-millisecond timeouts
	// positive instead of accidentally selecting the SDK's default timeout.
	timeoutMS := int(cfg.Timeout.Duration / time.Millisecond)
	if cfg.Timeout.Duration%time.Millisecond != 0 {
		timeoutMS++
	}
	sdkConfig := &openapi.Config{
		Endpoint:        dara.String(cfg.Endpoint),
		RegionId:        dara.String(cfg.RegionID),
		AccessKeyId:     dara.String(cfg.AccessKeyID),
		AccessKeySecret: dara.String(cfg.AccessKeySecret),
		ConnectTimeout:  dara.Int(timeoutMS),
		ReadTimeout:     dara.Int(timeoutMS),
		// OpenAPI v2.2.4 uses Config.RetryOptions, not RuntimeOptions.Autoretry.
		RetryOptions: &dara.RetryOptions{Retryable: false, MaxAttempts: 1},
	}
	if cfg.SecurityToken != "" {
		sdkConfig.SecurityToken = dara.String(cfg.SecurityToken)
	}
	client, err := alidnssdk.NewClient(sdkConfig)
	if err != nil {
		return nil, err
	}
	return &sdkAPI{client: client, httpClient: &http.Client{
		Timeout: cfg.Timeout.Duration,
	}}, nil
}

// redirectError is a permanent endpoint/protocol failure, not a network error.
type redirectError struct {
	status   int
	location string
}

type noRedirectTransport struct{ *http.Transport }

func (t *noRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.Transport.RoundTrip(req)
	if err == nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// Reject before net/http parses Location, which could otherwise produce a
		// credential-bearing url.Error classified as a network failure.
		resp.Body.Close()
		return nil, newRedirectError(resp)
	}
	return resp, err
}

func (e *redirectError) Error() string {
	return fmt.Sprintf("HTTP %d redirect refused (Location: %q)", e.status, e.location)
}

func newRedirectError(resp *http.Response) *redirectError {
	location := "<missing>"
	if resp.Header.Get("Location") != "" {
		location = "<invalid>"
		if target, err := resp.Location(); err == nil && target.Opaque == "" && (target.Scheme == "http" || target.Scheme == "https") {
			// Omit userinfo, signatures, security tokens and fragments.
			target.User = nil
			target.RawQuery, target.Fragment, target.RawFragment = "", "", ""
			target.ForceQuery = false
			location = target.String()
		}
	}
	return &redirectError{status: resp.StatusCode, location: location}
}

// The generated methods have no context parameter. Bind a context through the
// SDK's HTTP hook using a per-call client copy; the shared client stays immutable.
func (s *sdkAPI) clientFor(ctx context.Context) *alidnssdk.Client {
	client := *s.client
	client.HttpClient = contextHTTPClient{ctx: ctx, api: s}
	return &client
}

type contextHTTPClient struct {
	ctx context.Context
	api *sdkAPI
}

func (c contextHTTPClient) Call(request *http.Request, transport *http.Transport) (*http.Response, error) {
	// Reuse the first SDK-configured transport for connection pooling. All calls
	// use the same provider endpoint and timeout configuration.
	c.api.transportOnce.Do(func() {
		// Own the pool rather than mutating an SDK-supplied transport. Expiry
		// bounds resource retention even for callers that do not explicitly close.
		pool := transport.Clone()
		pool.IdleConnTimeout = 90 * time.Second
		pool.MaxIdleConns = 16
		pool.MaxIdleConnsPerHost = 4
		c.api.httpClient.Transport = &noRedirectTransport{pool}
		c.api.idleTransport.Store(pool)
	})
	response, err := c.api.httpClient.Do(request.Clone(c.ctx))
	var redirect *redirectError
	if errors.As(err, &redirect) {
		return nil, redirect // Strip url.Error without changing network error chains.
	}
	return response, err
}

func (s *sdkAPI) CloseIdleConnections() {
	if transport := s.idleTransport.Load(); transport != nil {
		transport.CloseIdleConnections()
	}
}

func (s *sdkAPI) describe(ctx context.Context, request describeRequest) (describeResult, error) {
	if err := ctx.Err(); err != nil {
		return describeResult{}, err
	}
	response, err := s.clientFor(ctx).DescribeSubDomainRecordsWithOptions(&alidnssdk.DescribeSubDomainRecordsRequest{
		DomainName: dara.String(request.Domain),
		SubDomain:  dara.String(request.SubDomain),
		Type:       dara.String(request.Type),
		PageSize:   dara.Int64(request.PageSize),
		PageNumber: dara.Int64(request.PageNumber),
	}, &dara.RuntimeOptions{})
	if err != nil {
		return describeResult{}, err
	}
	if response == nil || response.Body == nil {
		return describeResult{}, errors.New("DescribeSubDomainRecords returned an empty response")
	}
	body := response.Body
	result := describeResult{
		TotalCount:    dara.Int64Value(body.TotalCount),
		HasTotalCount: body.TotalCount != nil,
		HasRecords:    body.DomainRecords != nil && body.DomainRecords.Record != nil,
	}
	if result.HasRecords {
		result.Records = make([]remoteRecord, 0, len(body.DomainRecords.Record))
		for i, record := range body.DomainRecords.Record {
			if record == nil {
				return describeResult{}, fmt.Errorf("DescribeSubDomainRecords record %d is nil; refusing to write", i+1)
			}
			candidate := remoteRecord{
				RecordID: dara.StringValue(record.RecordId), RR: dara.StringValue(record.RR),
				Type: dara.StringValue(record.Type), Value: dara.StringValue(record.Value),
				TTL: dara.Int64Value(record.TTL), Line: dara.StringValue(record.Line), Status: dara.StringValue(record.Status),
			}
			if err := candidate.validate(); err != nil {
				return describeResult{}, fmt.Errorf("DescribeSubDomainRecords record %d: %w; refusing to write", i+1, err)
			}
			result.Records = append(result.Records, candidate)
		}
	}
	return result, nil
}

func (s *sdkAPI) add(ctx context.Context, r dns.Record) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	response, err := s.clientFor(ctx).AddDomainRecordWithOptions(&alidnssdk.AddDomainRecordRequest{
		DomainName: dara.String(r.Domain), RR: dara.String(r.RR), Type: dara.String(r.Type),
		Value: dara.String(r.Value), TTL: dara.Int64(int64(r.TTL)), Line: dara.String(r.Line),
	}, &dara.RuntimeOptions{})
	if err != nil {
		return "", err
	}
	if response == nil || response.Body == nil || dara.StringValue(response.Body.RecordId) == "" {
		return "", errors.New("AddDomainRecord returned an empty record ID")
	}
	return dara.StringValue(response.Body.RecordId), nil
}

func (s *sdkAPI) update(ctx context.Context, id string, r dns.Record) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	response, err := s.clientFor(ctx).UpdateDomainRecordWithOptions(&alidnssdk.UpdateDomainRecordRequest{
		RecordId: dara.String(id), RR: dara.String(r.RR), Type: dara.String(r.Type),
		Value: dara.String(r.Value), TTL: dara.Int64(int64(r.TTL)), Line: dara.String(r.Line),
	}, &dara.RuntimeOptions{})
	if err != nil {
		return "", err
	}
	if response == nil || response.Body == nil || dara.StringValue(response.Body.RecordId) == "" {
		return "", errors.New("UpdateDomainRecord returned an empty record ID")
	}
	return dara.StringValue(response.Body.RecordId), nil
}

func errorDetails(err error) (string, int, bool) {
	var coded codedError
	if errors.As(err, &coded) {
		return coded.ErrorCode(), coded.HTTPStatus(), true
	}
	var sdkErr *dara.SDKError
	if errors.As(err, &sdkErr) {
		// dara.SDKError has GetCode(), but exposes status only as a field.
		return dara.StringValue(sdkErr.GetCode()), dara.IntValue(sdkErr.StatusCode), true
	}
	var teaErr *tea.SDKError
	if errors.As(err, &teaErr) {
		// The pinned OpenAPI client converts service errors to this legacy type.
		return dara.StringValue(teaErr.Code), dara.IntValue(teaErr.StatusCode), true
	}
	var responseErr interface {
		GetCode() *string
		GetStatusCode() *int
	}
	if errors.As(err, &responseErr) {
		return dara.StringValue(responseErr.GetCode()), dara.IntValue(responseErr.GetStatusCode()), true
	}
	return "", 0, false
}
