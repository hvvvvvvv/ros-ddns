// Package alidns implements the Aliyun Alidns DNS provider.
package alidns

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
)

const (
	pageSize    = 100
	maxAttempts = 3 // Includes the initial request; never sleep after the last failure.
)

type describeRequest struct {
	Domain, SubDomain, Type string
	PageSize, PageNumber    int64
}

type remoteRecord struct {
	RecordID, RR, Type, Value, Line, Status string
	TTL                                     int64
}

type describeResult struct {
	Records    []remoteRecord
	TotalCount int64
}

// api deliberately contains no SDK models or status/delete operations.
type api interface {
	describe(context.Context, describeRequest) (describeResult, error)
	add(context.Context, dns.Record) (string, error)
	update(context.Context, string, dns.Record) (string, error)
}

type provider struct {
	api     api
	limiter *rate.Limiter
	sleep   func(context.Context, time.Duration) error
}

var _ dns.Provider = (*provider)(nil)

// New constructs a provider from normalized configuration without making requests.
func New(cfg config.ProviderConfig) (dns.Provider, error) {
	switch {
	case strings.TrimSpace(cfg.Endpoint) == "":
		return nil, errors.New("aliyun: endpoint must not be empty")
	case cfg.Timeout.Duration <= 0:
		return nil, errors.New("aliyun: timeout must be greater than zero")
	case cfg.QPS < 1:
		return nil, errors.New("aliyun: qps must be at least 1")
	}
	client, err := newSDKAPI(cfg)
	if err != nil {
		return nil, fmt.Errorf("aliyun: initialize SDK: %w", err)
	}
	return &provider{
		api:     client,
		limiter: rate.NewLimiter(rate.Limit(cfg.QPS), 1),
		sleep:   sleepContext,
	}, nil
}

func (*provider) Name() string { return "aliyun" }

// CloseIdleConnections releases idle sockets without disabling provider reuse.
func (p *provider) CloseIdleConnections() {
	if closer, ok := p.api.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (p *provider) Ensure(ctx context.Context, r dns.Record, recordIDHint string) (dns.EnsureResult, error) {
	matches, err := p.findRecords(ctx, r)
	if err != nil {
		return dns.EnsureResult{}, err
	}
	if len(matches) == 0 {
		var id string
		err := p.call(ctx, "AddDomainRecord", func() error {
			var err error
			id, err = p.api.add(ctx, r)
			return err
		})
		if err != nil {
			return dns.EnsureResult{}, err
		}
		slog.InfoContext(ctx, "Aliyun DNS record added", "fqdn", r.FQDN(), "type", r.Type, "value", r.Value, "record_id", id)
		return dns.EnsureResult{RecordID: id, Value: r.Value, Changed: true}, nil
	}

	selected := matches[0]
	if len(matches) > 1 {
		found := false
		for _, candidate := range matches {
			if recordIDHint != "" && candidate.RecordID == recordIDHint {
				selected, found = candidate, true
				break
			}
		}
		if !found {
			return dns.EnsureResult{}, fmt.Errorf("aliyun: %s %s line %q has %d matches: %w; remove duplicates or use a distinct line",
				r.FQDN(), r.Type, r.Line, len(matches), dns.ErrAmbiguousRecord)
		}
		slog.WarnContext(ctx, "Multiple Aliyun DNS records match; using record ID hint", "fqdn", r.FQDN(), "type", r.Type,
			"line", r.Line, "count", len(matches), "record_id", selected.RecordID)
	}
	return p.ensureRecord(ctx, r, selected)
}

func (p *provider) findRecords(ctx context.Context, r dns.Record) ([]remoteRecord, error) {
	var matches []remoteRecord
	var seen int64
	for page := int64(1); ; page++ {
		var result describeResult
		err := p.call(ctx, "DescribeSubDomainRecords", func() error {
			var err error
			result, err = p.api.describe(ctx, describeRequest{
				Domain: r.Domain, SubDomain: r.FQDN(), Type: r.Type, PageSize: pageSize, PageNumber: page,
			})
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, candidate := range result.Records {
			// Line is part of the binding identity. A hint must never select another line.
			if candidate.Line == r.Line && strings.EqualFold(candidate.RR, r.RR) && strings.EqualFold(candidate.Type, r.Type) {
				matches = append(matches, candidate)
			}
		}
		seen += int64(len(result.Records))
		if result.TotalCount > 0 {
			if seen >= result.TotalCount {
				return matches, nil
			}
			if len(result.Records) == 0 {
				return nil, errors.New("aliyun: DescribeSubDomainRecords returned an incomplete record list; refusing to write")
			}
		} else if len(result.Records) < pageSize {
			return matches, nil
		}
	}
}

func (p *provider) ensureRecord(ctx context.Context, r dns.Record, current remoteRecord) (dns.EnsureResult, error) {
	if current.RecordID == "" {
		return dns.EnsureResult{}, errors.New("aliyun: matching record has an empty record ID; refusing to write")
	}
	if strings.EqualFold(current.Status, "DISABLE") {
		slog.WarnContext(ctx, "Aliyun DNS record is disabled; preserving status", "fqdn", r.FQDN(), "type", r.Type, "record_id", current.RecordID)
	}
	if normalizeValue(current.Value) == normalizeValue(r.Value) && current.TTL == int64(r.TTL) && current.Line == r.Line {
		return dns.EnsureResult{RecordID: current.RecordID, Value: normalizeValue(current.Value)}, nil
	}
	var id string
	err := p.call(ctx, "UpdateDomainRecord", func() error {
		var err error
		id, err = p.api.update(ctx, current.RecordID, r)
		return err
	})
	if err != nil {
		return dns.EnsureResult{}, err
	}
	slog.InfoContext(ctx, "Aliyun DNS record updated", "fqdn", r.FQDN(), "type", r.Type, "value", r.Value, "record_id", id)
	return dns.EnsureResult{RecordID: id, Value: r.Value, PreviousValue: current.Value, Changed: true}, nil
}

func normalizeValue(value string) string {
	if addr, err := netip.ParseAddr(value); err == nil {
		return addr.String()
	}
	return value
}

// call rate-limits every request, including retries, across all Ensure calls.
func (p *provider) call(ctx context.Context, operation string, fn func() error) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("aliyun: %s: %w", operation, err)
		}
		if err := p.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("aliyun: %s rate limit: %w", operation, err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("aliyun: %s: %w", operation, err)
		}
		err := fn()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("aliyun: %s: %w", operation, ctx.Err())
		}
		if attempt+1 >= maxAttempts || !retryable(err) {
			return fmt.Errorf("aliyun: %s: %w", operation, err)
		}
		if err := p.sleep(ctx, retryDelay(attempt)); err != nil {
			return fmt.Errorf("aliyun: %s backoff: %w", operation, err)
		}
	}
}

func retryDelay(attempt int) time.Duration {
	base := time.Second << min(attempt, 2) // 1s, 2s, 4s, plus up to 25% jitter.
	return base + time.Duration(rand.Int64N(int64(base/4)+1))
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// codedError also allows fakes to expose classification without importing an SDK.
type codedError interface {
	error
	ErrorCode() string
	HTTPStatus() int
}

func retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if code, status, ok := errorDetails(err); ok {
		return strings.Contains(code, "Throttling") || (status >= 500 && status < 600)
	}
	var networkErr net.Error
	return errors.As(err, &networkErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
