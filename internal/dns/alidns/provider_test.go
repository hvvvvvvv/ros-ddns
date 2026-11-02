package alidns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alibabacloud-go/tea/dara"
	"github.com/alibabacloud-go/tea/tea"
	"golang.org/x/time/rate"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
)

type updateCall struct {
	id     string
	record dns.Record
}

type fakeAPI struct {
	records    []remoteRecord
	describes  []describeRequest
	adds       []dns.Record
	updates    []updateCall
	describeFn func(context.Context, describeRequest) (describeResult, error)
	addFn      func(context.Context, dns.Record) (string, error)
	updateFn   func(context.Context, string, dns.Record) (string, error)
}

func (f *fakeAPI) describe(ctx context.Context, r describeRequest) (describeResult, error) {
	f.describes = append(f.describes, r)
	if f.describeFn != nil {
		return f.describeFn(ctx, r)
	}
	return describeResult{Records: f.records, TotalCount: int64(len(f.records))}, nil
}

func (f *fakeAPI) add(ctx context.Context, r dns.Record) (string, error) {
	f.adds = append(f.adds, r)
	if f.addFn != nil {
		return f.addFn(ctx, r)
	}
	return "added-id", nil
}

func (f *fakeAPI) update(ctx context.Context, id string, r dns.Record) (string, error) {
	f.updates = append(f.updates, updateCall{id: id, record: r})
	if f.updateFn != nil {
		return f.updateFn(ctx, id, r)
	}
	return id, nil
}

func testProvider(f *fakeAPI) *provider {
	return &provider{api: f, limiter: rate.NewLimiter(rate.Inf, 1), sleep: func(context.Context, time.Duration) error { return nil }}
}

func testRecord() (dns.Record, remoteRecord) {
	r := dns.Record{Domain: "example.com", RR: "home", Type: "A", Value: "192.0.2.10", TTL: 600, Line: "default"}
	current := remoteRecord{RecordID: "existing-id", RR: r.RR, Type: r.Type, Value: r.Value, TTL: int64(r.TTL), Line: r.Line, Status: "ENABLE"}
	return r, current
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestEnsureExistingRecord(t *testing.T) {
	for _, name := range []string{"no-op", "value differs", "TTL differs", "IPv6 canonicalization", "DISABLE still updates", "DISABLE no-op"} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			r, current := testRecord()
			changed := false
			switch name {
			case "value differs":
				current.Value, changed = "192.0.2.11", true
			case "TTL differs":
				current.TTL, changed = 300, true
			case "IPv6 canonicalization":
				r.Type, current.Type = "AAAA", "AAAA"
				r.Value, current.Value = "2001:db8::abcd", "2001:0DB8:0000:0000:0000:0000:0000:ABCD"
			case "DISABLE still updates":
				current.Status, current.Value, changed = "DISABLE", "192.0.2.12", true
			case "DISABLE no-op":
				current.Status = "DISABLE"
			}
			f := &fakeAPI{records: []remoteRecord{current}}
			got, err := testProvider(f).Ensure(context.Background(), r, "")
			if err != nil {
				t.Fatal(err)
			}
			want := dns.EnsureResult{RecordID: current.RecordID, Value: r.Value, Changed: changed}
			if changed {
				want.PreviousValue = current.Value
				if len(f.updates) != 1 || f.updates[0] != (updateCall{id: current.RecordID, record: r}) {
					t.Fatalf("updates = %+v", f.updates)
				}
			} else if len(f.updates) != 0 {
				t.Fatalf("no-op performed updates: %+v", f.updates)
			}
			if got != want || len(f.adds) != 0 || len(f.describes) != 1 {
				t.Fatalf("result = %+v, want %+v; calls = %+v", got, want, f)
			}
			wantQuery := describeRequest{Domain: r.Domain, SubDomain: r.FQDN(), Type: r.Type, PageSize: 100, PageNumber: 1}
			if f.describes[0] != wantQuery {
				t.Fatalf("describe request = %+v, want %+v", f.describes[0], wantQuery)
			}
			if current.Status == "DISABLE" && (!strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "disabled")) {
				t.Fatalf("missing disabled warning: %s", logs)
			}
			if changed && (!strings.Contains(logs.String(), r.FQDN()) || !strings.Contains(logs.String(), r.Value) || !strings.Contains(logs.String(), "type="+r.Type)) {
				t.Fatalf("missing change log: %s", logs)
			}
		})
	}
}

func TestEnsureAddsWhenNoMatch(t *testing.T) {
	for _, name := range []string{"empty", "other RR", "other type", "other line", "hint on other line", "apex"} {
		t.Run(name, func(t *testing.T) {
			r, current := testRecord()
			f := &fakeAPI{}
			hint := ""
			switch name {
			case "other RR":
				current.RR = "elsewhere"
				f.records = []remoteRecord{current}
			case "other type":
				current.Type = "AAAA"
				f.records = []remoteRecord{current}
			case "other line", "hint on other line":
				current.Line = "telecom"
				f.records = []remoteRecord{current}
				if name == "hint on other line" {
					hint = current.RecordID
				}
			case "apex":
				r.RR = "@"
			}
			got, err := testProvider(f).Ensure(context.Background(), r, hint)
			if err != nil {
				t.Fatal(err)
			}
			want := dns.EnsureResult{RecordID: "added-id", Value: r.Value, Changed: true}
			if got != want || len(f.adds) != 1 || f.adds[0] != r || len(f.updates) != 0 {
				t.Fatalf("result = %+v, want %+v; calls = %+v", got, want, f)
			}
			if name == "apex" && f.describes[0].SubDomain != r.Domain {
				t.Fatalf("apex query = %+v", f.describes[0])
			}
		})
	}
}

func TestEnsureFiltersBeforeSelectingHint(t *testing.T) {
	r, current := testRecord()
	other := current
	other.RecordID, other.Line = "other-line-id", "telecom"
	f := &fakeAPI{records: []remoteRecord{other, current}}
	got, err := testProvider(f).Ensure(context.Background(), r, other.RecordID)
	if err != nil || got.RecordID != current.RecordID || got.Changed || len(f.adds)+len(f.updates) != 0 {
		t.Fatalf("result = %+v, error = %v, calls = %+v", got, err, f)
	}
}

func TestEnsureRecordUpdatesLine(t *testing.T) {
	// Ensure filters by line first. Test the selected-record comparison directly
	// so its line guard is covered without allowing cross-line hint migration.
	r, current := testRecord()
	current.Line = "telecom"
	f := &fakeAPI{}
	got, err := testProvider(f).ensureRecord(context.Background(), r, current)
	if err != nil || !got.Changed || got.PreviousValue != current.Value || len(f.updates) != 1 || f.updates[0].record.Line != r.Line {
		t.Fatalf("result = %+v, error = %v, updates = %+v", got, err, f.updates)
	}
}

func TestEnsureAmbiguousRecords(t *testing.T) {
	for _, hint := range []string{"", "stale-id", "second-id"} {
		t.Run("hint="+hint, func(t *testing.T) {
			logs := captureLogs(t)
			r, first := testRecord()
			first.Value = "192.0.2.11"
			second := first
			second.RecordID, second.Value = "second-id", "192.0.2.12"
			f := &fakeAPI{records: []remoteRecord{first, second}}
			got, err := testProvider(f).Ensure(context.Background(), r, hint)
			if hint != second.RecordID {
				if !errors.Is(err, dns.ErrAmbiguousRecord) || !strings.Contains(err.Error(), "2 matches") || !strings.Contains(err.Error(), "distinct line") {
					t.Fatalf("expected actionable ambiguity error, got %v", err)
				}
				if len(f.adds)+len(f.updates) != 0 {
					t.Fatalf("ambiguous records caused writes: %+v", f)
				}
				return
			}
			want := dns.EnsureResult{RecordID: second.RecordID, Value: r.Value, PreviousValue: second.Value, Changed: true}
			if err != nil || got != want || len(f.updates) != 1 || f.updates[0].id != second.RecordID || len(f.adds) != 0 {
				t.Fatalf("result = %+v, error = %v, calls = %+v", got, err, f)
			}
			if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "count=2") {
				t.Fatalf("missing duplicate warning: %s", logs)
			}
		})
	}
}

type fakeError struct {
	code   string
	status int
}

func (e *fakeError) Error() string     { return e.code }
func (e *fakeError) ErrorCode() string { return e.code }
func (e *fakeError) HTTPStatus() int   { return e.status }

func TestEnsureRetries(t *testing.T) {
	for _, operation := range []string{"describe", "add", "update"} {
		for _, failure := range []string{"throttling then success", "validation", "exhausted"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				r, current := testRecord()
				f := &fakeAPI{}
				if operation == "update" {
					current.Value = "192.0.2.11"
					f.records = []remoteRecord{current}
				}
				failureErr := &fakeError{code: "Throttling.User", status: 400}
				if failure == "validation" {
					failureErr.code = "InvalidParameter.Value"
				}
				calls := 0
				nextError := func() error {
					calls++
					if failure != "throttling then success" || calls == 1 {
						return fmt.Errorf("wrapped: %w", failureErr)
					}
					return nil
				}
				switch operation {
				case "describe":
					f.describeFn = func(context.Context, describeRequest) (describeResult, error) { return describeResult{}, nextError() }
				case "add":
					f.addFn = func(context.Context, dns.Record) (string, error) { return "added-id", nextError() }
				case "update":
					f.updateFn = func(_ context.Context, id string, _ dns.Record) (string, error) { return id, nextError() }
				}
				p := testProvider(f)
				var delays []time.Duration
				p.sleep = func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil }
				got, err := p.Ensure(context.Background(), r, "")
				wantCalls, wantSleeps := 2, 1
				switch failure {
				case "throttling then success":
					if err != nil || !got.Changed || got.Value != r.Value {
						t.Fatalf("result = %+v, error = %v", got, err)
					}
				case "validation":
					wantCalls, wantSleeps = 1, 0
					if !errors.Is(err, failureErr) {
						t.Fatalf("error = %v", err)
					}
				case "exhausted":
					wantCalls, wantSleeps = 3, 2
					if !errors.Is(err, failureErr) {
						t.Fatalf("error = %v", err)
					}
				}
				if calls != wantCalls || len(delays) != wantSleeps {
					t.Fatalf("calls = %d, sleeps = %v; want %d calls, %d sleeps", calls, delays, wantCalls, wantSleeps)
				}
				for i, delay := range delays {
					base := time.Second << i
					if delay < base || delay > base+base/4 {
						t.Fatalf("delay %d = %v", i, delay)
					}
				}
				if operation != "describe" && len(f.describes) != 1 {
					t.Fatalf("write retry repeated describe: %+v", f.describes)
				}
				if operation == "describe" && err != nil && len(f.adds)+len(f.updates) != 0 {
					t.Fatal("failed describe caused writes")
				}
			})
		}
	}
}

func TestRetryClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"fake throttling", &fakeError{code: "Throttling.User", status: 400}, true},
		{"HTTP 500", &fakeError{code: "InternalError", status: 500}, true},
		{"HTTP 599", &fakeError{status: 599}, true},
		{"validation", &fakeError{code: "InvalidParameter", status: 400}, false},
		{"permission", &fakeError{code: "Forbidden", status: 403}, false},
		{"dara throttling", &dara.SDKError{Code: dara.String("Throttling"), StatusCode: dara.Int(400)}, true},
		{"dara HTTP 503", &dara.SDKError{Code: dara.String("ServiceUnavailable"), StatusCode: dara.Int(503)}, true},
		{"dara validation", &dara.SDKError{Code: dara.String("InvalidParameter"), StatusCode: dara.Int(400)}, false},
		{"dara empty", &dara.SDKError{}, false},
		{"tea throttling", &tea.SDKError{Code: dara.String("Throttling.User"), StatusCode: dara.Int(400)}, true},
		{"tea HTTP 502", &tea.SDKError{StatusCode: dara.Int(502)}, true},
		{"timeout", &net.DNSError{IsTimeout: true}, true},
		{"network", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection reset")}, true},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"request timeout", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"unknown", errors.New("invalid request"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryable(fmt.Errorf("outer: %w", tc.err)); got != tc.want {
				t.Fatalf("retryable = %v, want %v", got, tc.want)
			}
		})
	}
	if retryable(nil) {
		t.Fatal("nil error is retryable")
	}
}

func TestEnsureChecksAllPagesBeforeWriting(t *testing.T) {
	for _, hint := range []string{"", "last-id"} {
		t.Run("hint="+hint, func(t *testing.T) {
			r, current := testRecord()
			firstPage := make([]remoteRecord, 100)
			for i := range firstPage {
				firstPage[i] = remoteRecord{RR: "unrelated", Type: r.Type, Line: r.Line}
			}
			firstPage[0] = current
			last := current
			last.RecordID, last.Value = "last-id", "192.0.2.11"
			f := &fakeAPI{}
			f.describeFn = func(_ context.Context, q describeRequest) (describeResult, error) {
				if q.PageSize != 100 {
					t.Fatalf("page size = %d", q.PageSize)
				}
				switch q.PageNumber {
				case 1:
					return describeResult{Records: firstPage, TotalCount: 101}, nil
				case 2:
					return describeResult{Records: []remoteRecord{last}, TotalCount: 101}, nil
				default:
					t.Fatalf("unexpected page %d", q.PageNumber)
					return describeResult{}, nil
				}
			}
			got, err := testProvider(f).Ensure(context.Background(), r, hint)
			if hint == "" {
				if !errors.Is(err, dns.ErrAmbiguousRecord) || len(f.adds)+len(f.updates) != 0 {
					t.Fatalf("error = %v; calls = %+v", err, f)
				}
			} else if err != nil || got.RecordID != last.RecordID || len(f.updates) != 1 || f.updates[0].id != last.RecordID {
				t.Fatalf("result = %+v, error = %v, updates = %+v", got, err, f.updates)
			}
			if len(f.describes) != 2 {
				t.Fatalf("describe calls = %d", len(f.describes))
			}
		})
	}
}

func TestEnsureIncompletePageDoesNotWrite(t *testing.T) {
	r, _ := testRecord()
	f := &fakeAPI{describeFn: func(context.Context, describeRequest) (describeResult, error) {
		return describeResult{TotalCount: 1}, nil
	}}
	_, err := testProvider(f).Ensure(context.Background(), r, "")
	if err == nil || len(f.adds)+len(f.updates) != 0 {
		t.Fatalf("error = %v, calls = %+v", err, f)
	}
}

func TestEnsureCancellation(t *testing.T) {
	for _, stage := range []string{"before request", "backoff", "before write"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r, _ := testRecord()
			f := &fakeAPI{}
			p := testProvider(f)
			switch stage {
			case "before request":
				cancel()
			case "backoff":
				f.describeFn = func(context.Context, describeRequest) (describeResult, error) {
					return describeResult{}, &fakeError{code: "Throttling"}
				}
				p.sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return sleepContext(ctx, time.Hour) }
			case "before write":
				f.describeFn = func(context.Context, describeRequest) (describeResult, error) { cancel(); return describeResult{}, nil }
			}
			_, err := p.Ensure(ctx, r, "")
			if !errors.Is(err, context.Canceled) || len(f.adds)+len(f.updates) != 0 || len(f.describes) > 1 {
				t.Fatalf("error = %v, calls = %+v", err, f)
			}
		})
	}
}

func TestEnsureRateLimitsEveryRequest(t *testing.T) {
	for _, scenario := range []string{"shared across Ensure", "write", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			r, current := testRecord()
			f := &fakeAPI{}
			p := testProvider(f)
			p.limiter = rate.NewLimiter(rate.Every(time.Hour), 1)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			switch scenario {
			case "shared across Ensure":
				f.records = []remoteRecord{current}
				if _, err := p.Ensure(ctx, r, ""); err != nil {
					t.Fatal(err)
				}
			case "retry":
				f.describeFn = func(context.Context, describeRequest) (describeResult, error) {
					return describeResult{}, &fakeError{code: "Throttling"}
				}
			}
			_, err := p.Ensure(ctx, r, "")
			if err == nil || !strings.Contains(err.Error(), "rate limit") || len(f.describes) != 1 || len(f.adds)+len(f.updates) != 0 {
				t.Fatalf("error = %v, calls = %+v", err, f)
			}
		})
	}
}

func TestNew(t *testing.T) {
	valid := config.ProviderConfig{Endpoint: "alidns.aliyuncs.com", RegionID: "cn-hangzhou", AccessKeyID: "test-key", AccessKeySecret: "test-secret", Timeout: config.Duration{Duration: 1500 * time.Millisecond}, QPS: 5}
	for _, token := range []string{"", "test-token"} {
		cfg := valid
		cfg.SecurityToken = token
		result, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		p := result.(*provider)
		s := p.api.(*sdkAPI)
		if p.Name() != "aliyun" || p.limiter.Limit() != 5 || p.limiter.Burst() < 1 || dara.StringValue(s.client.Endpoint) != cfg.Endpoint || dara.StringValue(s.client.RegionId) != cfg.RegionID {
			t.Fatal("provider name, rate or SDK endpoint/region not configured")
		}
		if dara.IntValue(s.client.ConnectTimeout) != 1500 || dara.IntValue(s.client.ReadTimeout) != 1500 || s.client.RetryOptions.Retryable || s.client.Credential == nil {
			t.Fatal("SDK timeouts, retry policy or credentials not configured")
		}
	}
	// Missing credentials must not prevent the provider (and therefore the
	// daemon) from starting; authentication fails at synchronization time.
	empty := valid
	empty.AccessKeyID, empty.AccessKeySecret = "", ""
	if _, err := New(empty); err != nil {
		t.Fatalf("New with empty credentials: %v", err)
	}
	for _, field := range []string{"endpoint", "timeout", "qps"} {
		t.Run("invalid "+field, func(t *testing.T) {
			cfg := valid
			switch field {
			case "endpoint":
				cfg.Endpoint = " "
			case "timeout":
				cfg.Timeout.Duration = 0
			case "qps":
				cfg.QPS = 0
			}
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), field) || strings.Contains(err.Error(), valid.AccessKeySecret) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSDKContextBindingWithoutNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &sdkAPI{httpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Context() != ctx {
			t.Fatal("Ensure context was not propagated to HTTP request")
		}
		cancel()
		return nil, r.Context().Err()
	})}}
	s.transportOnce.Do(func() {}) // Keep the fake transport; no sockets are opened.
	request, err := http.NewRequest(http.MethodPost, "https://alidns.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (contextHTTPClient{ctx: ctx, api: s}).Call(request, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
