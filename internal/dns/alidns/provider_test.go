package alidns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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
	return completeDescribe(f.records, int64(len(f.records))), nil
}

func completeDescribe(records []remoteRecord, total int64) describeResult {
	return describeResult{Records: records, TotalCount: total, HasTotalCount: true, HasRecords: true}
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
			want := dns.EnsureResult{RecordID: current.RecordID, Changed: changed}
			if changed {
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
			want := dns.EnsureResult{RecordID: "added-id", Changed: true}
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
	if err != nil || !got.Changed || got.RecordID != current.RecordID || len(f.updates) != 1 || f.updates[0].record.Line != r.Line {
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
			want := dns.EnsureResult{RecordID: second.RecordID, Changed: true}
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
					f.describeFn = func(context.Context, describeRequest) (describeResult, error) {
						return completeDescribe(nil, 0), nextError()
					}
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
					wantID := "added-id"
					if operation == "update" {
						wantID = current.RecordID
					}
					if err != nil || !got.Changed || got.RecordID != wantID {
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
		{"redirect", &redirectError{status: http.StatusFound, location: "https://example.com/"}, false},
		{"wrapped redirect", &url.Error{Op: "Post", URL: "https://example.com/", Err: &redirectError{status: http.StatusTemporaryRedirect}}, false},
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
				firstPage[i] = current
				firstPage[i].RecordID, firstPage[i].RR = fmt.Sprintf("unrelated-%d", i), "unrelated"
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
					return completeDescribe(firstPage, 101), nil
				case 2:
					return completeDescribe([]remoteRecord{last}, 101), nil
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

func TestEnsureRejectsIncompleteRecords(t *testing.T) {
	for _, field := range []string{"RecordId", "RR", "Type", "Line", "Value", "TTL"} {
		for _, unmatched := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/unmatched=%t", field, unmatched), func(t *testing.T) {
				r, current := testRecord()
				if unmatched {
					current.RR = "unrelated"
				}
				switch field {
				case "RecordId":
					current.RecordID = ""
				case "RR":
					current.RR = ""
				case "Type":
					current.Type = ""
				case "Line":
					current.Line = ""
				case "Value":
					current.Value = " "
				case "TTL":
					current.TTL = 0
				}
				f := &fakeAPI{records: []remoteRecord{current}}
				got, err := testProvider(f).Ensure(context.Background(), r, "")
				if err == nil || !strings.Contains(err.Error(), field) || got != (dns.EnsureResult{}) || len(f.adds)+len(f.updates) != 0 {
					t.Fatalf("result = %+v, error = %v, writes = %d", got, err, len(f.adds)+len(f.updates))
				}
			})
		}
	}
}

func TestEnsureRejectsIncompletePages(t *testing.T) {
	r, current := testRecord()
	full := make([]remoteRecord, pageSize)
	for i := range full {
		full[i] = current
		full[i].RecordID = fmt.Sprintf("record-%d", i)
	}
	one := []remoteRecord{current}
	for _, tc := range []struct {
		name  string
		pages []describeResult
	}{
		{"empty object", []describeResult{{}}},
		{"missing first count", []describeResult{{HasRecords: true}}},
		{"missing first records", []describeResult{{HasTotalCount: true}}},
		{"negative count", []describeResult{completeDescribe(nil, -1)}},
		{"zero count with records", []describeResult{completeDescribe(one, 0)}},
		{"empty positive first page", []describeResult{completeDescribe(nil, 1)}},
		{"too many records", []describeResult{completeDescribe(append(one, current), 1)}},
		{"short first page", []describeResult{completeDescribe(one, 101)}},
		{"oversized first page", []describeResult{completeDescribe(append(append([]remoteRecord{}, full...), current), 101)}},
		{"missing second count", []describeResult{completeDescribe(full, 101), {Records: one, HasRecords: true}}},
		{"missing second records", []describeResult{completeDescribe(full, 101), {TotalCount: 101, HasTotalCount: true}}},
		{"second count becomes zero", []describeResult{completeDescribe(full, 101), completeDescribe(nil, 0)}},
		{"second count increases", []describeResult{completeDescribe(full, 101), completeDescribe(one, 102)}},
		{"second count decreases", []describeResult{completeDescribe(full, 101), completeDescribe(nil, 100)}},
		{"empty second page", []describeResult{completeDescribe(full, 101), completeDescribe(nil, 101)}},
		{"excess final page", []describeResult{completeDescribe(full, 101), completeDescribe(append(one, current), 101)}},
		{"short final page", []describeResult{completeDescribe(full, 102), completeDescribe(one, 102)}},
		{"short middle page", []describeResult{completeDescribe(full, 201), completeDescribe(full[:99], 201)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAPI{describeFn: func(_ context.Context, q describeRequest) (describeResult, error) {
				if q.PageNumber > int64(len(tc.pages)) {
					t.Fatalf("requested unexpected page %d", q.PageNumber)
				}
				return tc.pages[q.PageNumber-1], nil
			}}
			p := testProvider(f)
			p.sleep = func(context.Context, time.Duration) error {
				t.Error("invalid response must not be retried")
				return nil
			}
			got, err := p.Ensure(context.Background(), r, current.RecordID)
			if err == nil || !strings.Contains(err.Error(), "refusing to write") || got != (dns.EnsureResult{}) || len(f.adds)+len(f.updates) != 0 {
				t.Fatalf("result = %+v, error = %v, writes = %d", got, err, len(f.adds)+len(f.updates))
			}
			if len(f.describes) != len(tc.pages) {
				t.Fatalf("describes = %d, want %d", len(f.describes), len(tc.pages))
			}
		})
	}
}

func TestEnsureCompletePagination(t *testing.T) {
	for _, total := range []int{0, 1, 100, 101, 200, 201} {
		for _, match := range []bool{false, true} {
			if total == 0 && match {
				continue
			}
			t.Run(fmt.Sprintf("total=%d/match=%t", total, match), func(t *testing.T) {
				r, current := testRecord()
				records := make([]remoteRecord, total)
				for i := range records {
					records[i] = current
					records[i].RecordID, records[i].RR = fmt.Sprintf("other-%d", i), "other"
				}
				if match {
					records[total-1] = current
				}
				f := &fakeAPI{describeFn: func(_ context.Context, q describeRequest) (describeResult, error) {
					start := int(q.PageNumber-1) * pageSize
					if start > total || (total > 0 && start == total) {
						t.Fatalf("unexpected page %d", q.PageNumber)
					}
					return completeDescribe(records[start:min(start+pageSize, total)], int64(total)), nil
				}}
				got, err := testProvider(f).Ensure(context.Background(), r, "")
				if err != nil || len(f.describes) != max(1, (total+pageSize-1)/pageSize) || len(f.updates) != 0 {
					t.Fatalf("result = %+v, error = %v, describes = %d, updates = %d", got, err, len(f.describes), len(f.updates))
				}
				if match {
					if got.RecordID != current.RecordID || got.Changed || len(f.adds) != 0 {
						t.Fatalf("matching result = %+v, adds = %d", got, len(f.adds))
					}
				} else if !got.Changed || got.RecordID != "added-id" || len(f.adds) != 1 || f.adds[0] != r {
					t.Fatalf("added result = %+v, adds = %d", got, len(f.adds))
				}
			})
		}
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
				f.describeFn = func(context.Context, describeRequest) (describeResult, error) {
					cancel()
					return completeDescribe(nil, 0), nil
				}
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
		if p.limiter.Limit() != 5 || p.limiter.Burst() < 1 || dara.StringValue(s.client.Endpoint) != cfg.Endpoint || dara.StringValue(s.client.RegionId) != cfg.RegionID {
			t.Fatal("provider rate or SDK endpoint/region not configured")
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

func testSDKAPI(t *testing.T, server *httptest.Server) *sdkAPI {
	t.Helper()
	s, err := newSDKAPI(config.ProviderConfig{
		Endpoint: strings.TrimPrefix(server.URL, "http://"), RegionID: "cn-hangzhou",
		AccessKeyID: "test-key", AccessKeySecret: "test-secret", SecurityToken: "test-token",
		Timeout: config.Duration{Duration: 3 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.client.Protocol = dara.String("HTTP")
	t.Cleanup(s.CloseIdleConnections)
	return s
}

func TestSDKRejectsRedirectsWithoutRetry(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, operation := range []string{"describe", "add", "update"} {
			t.Run(fmt.Sprintf("%d/%s", status, operation), func(t *testing.T) {
				var requests, redirected atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirected" {
						redirected.Add(1)
						_, _ = io.WriteString(w, `{"RecordId":"redirected-id","TotalCount":0,"DomainRecords":{"Record":[]}}`)
						return
					}
					requests.Add(1)
					if r.Method != http.MethodPost {
						t.Errorf("SDK method = %s, want POST", r.Method)
					}
					w.Header().Set("Location", "http://redirect-user:redirect-password@"+r.Host+"/redirected?AccessKeyId=test-key&SecurityToken=test-token&Signature=query-secret#fragment-secret")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "body-secret")
				}))
				t.Cleanup(server.Close)
				s := testSDKAPI(t, server)
				r, current := testRecord()
				f := &fakeAPI{}
				switch operation {
				case "describe":
					f.describeFn = s.describe
				case "add":
					f.addFn = s.add
				case "update":
					current.Value = "192.0.2.11"
					f.records, f.updateFn = []remoteRecord{current}, s.update
				}
				p := testProvider(f)
				sleeps := 0
				p.sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
				got, err := p.Ensure(context.Background(), r, "")
				var redirect *redirectError
				if !errors.As(err, &redirect) || redirect.status != status || retryable(err) || got != (dns.EnsureResult{}) {
					t.Fatalf("result = %+v, error = %v, want permanent redirect failure", got, err)
				}
				if requests.Load() != 1 || redirected.Load() != 0 || sleeps != 0 {
					t.Fatalf("requests = %d, redirected = %d, sleeps = %d; want 1, 0, 0", requests.Load(), redirected.Load(), sleeps)
				}
				if operation == "describe" && len(f.adds)+len(f.updates) != 0 {
					t.Fatal("redirected describe caused writes")
				}
				if !strings.Contains(err.Error(), server.URL+"/redirected") {
					t.Fatalf("error lacks sanitized Location: %v", err)
				}
				for _, secret := range []string{"test-key", "test-secret", "test-token", "redirect-user", "redirect-password", "query-secret", "fragment-secret", "body-secret"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("redirect error leaked %q: %v", secret, err)
					}
				}
			})
		}
	}
}

func TestSDKRejects3xxWithMissingOrInvalidLocation(t *testing.T) {
	for _, tc := range []struct {
		name, location string
		status         int
	}{
		{"302 without Location", "", http.StatusFound},
		{"307 without Location", "", http.StatusTemporaryRedirect},
		{"304", "", http.StatusNotModified},
		{"malformed Location", "http://test-key:test-secret@localhost/%zz?SecurityToken=test-token", http.StatusFound},
		{"opaque Location", "https:test-secret?SecurityToken=test-token", http.StatusTemporaryRedirect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				if tc.location != "" {
					w.Header().Set("Location", tc.location)
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(server.Close)
			f := &fakeAPI{describeFn: testSDKAPI(t, server).describe}
			r, _ := testRecord()
			got, err := testProvider(f).Ensure(context.Background(), r, "")
			var redirect *redirectError
			if !errors.As(err, &redirect) || redirect.status != tc.status || retryable(err) || requests.Load() != 1 || got.Changed || len(f.adds)+len(f.updates) != 0 {
				t.Fatalf("result = %+v, error = %v, requests = %d, writes = %d", got, err, requests.Load(), len(f.adds)+len(f.updates))
			}
			for _, secret := range []string{"test-key", "test-secret", "test-token"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked %q: %v", secret, err)
				}
			}
		})
	}
}

func TestSDKDescribeRejectsIncompleteResponses(t *testing.T) {
	cases := []struct{ name, body string }{
		{"empty object", `{}`},
		{"missing count", `{"DomainRecords":{"Record":[]}}`},
		{"null count", `{"TotalCount":null,"DomainRecords":{"Record":[]}}`},
		{"missing DomainRecords", `{"TotalCount":0}`},
		{"null DomainRecords", `{"TotalCount":0,"DomainRecords":null}`},
		{"missing Record array", `{"TotalCount":0,"DomainRecords":{}}`},
		{"null Record array", `{"TotalCount":0,"DomainRecords":{"Record":null}}`},
		{"nil record", `{"TotalCount":1,"DomainRecords":{"Record":[null]}}`},
	}
	for _, field := range []string{"RecordId", "RR", "Type", "Line", "Value", "TTL"} {
		for _, mode := range []string{"missing", "null", "empty"} {
			record := map[string]any{"RecordId": "id", "RR": "home", "Type": "A", "Line": "default", "Value": "192.0.2.10", "TTL": 600}
			switch mode {
			case "missing":
				delete(record, field)
			case "null":
				record[field] = nil
			case "empty":
				if field == "TTL" {
					record[field] = 0
				} else {
					record[field] = ""
				}
			}
			body, err := json.Marshal(map[string]any{"TotalCount": 1, "DomainRecords": map[string]any{"Record": []any{record}}})
			if err != nil {
				t.Fatal(err)
			}
			cases = append(cases, struct{ name, body string }{mode + " " + field, string(body)})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			f := &fakeAPI{describeFn: testSDKAPI(t, server).describe}
			p := testProvider(f)
			p.sleep = func(context.Context, time.Duration) error {
				t.Error("invalid response must not be retried")
				return nil
			}
			r, _ := testRecord()
			got, err := p.Ensure(context.Background(), r, "")
			if err == nil || !strings.Contains(err.Error(), "refusing to write") || got != (dns.EnsureResult{}) || len(f.adds)+len(f.updates) != 0 || requests.Load() != 1 {
				t.Fatalf("result = %+v, error = %v, writes = %d, requests = %d", got, err, len(f.adds)+len(f.updates), requests.Load())
			}
		})
	}
}

func TestSDKDescribeCompleteResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		count      int64
	}{
		{"explicit empty array", `{"TotalCount":0,"DomainRecords":{"Record":[]}}`, 0},
		{"single record", `{"TotalCount":1,"DomainRecords":{"Record":[{"RecordId":"id","RR":"home","Type":"A","Line":"default","Value":"192.0.2.10","TTL":600}]}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			s := testSDKAPI(t, server)
			got, err := s.describe(context.Background(), describeRequest{Domain: "example.com", SubDomain: "home.example.com", Type: "A", PageNumber: 1, PageSize: pageSize})
			if err != nil || !got.HasRecords || !got.HasTotalCount || got.TotalCount != tc.count || int64(len(got.Records)) != tc.count {
				t.Fatalf("describe = %+v, error = %v", got, err)
			}
			f := &fakeAPI{describeFn: s.describe}
			r, _ := testRecord()
			result, err := testProvider(f).Ensure(context.Background(), r, "")
			if err != nil || result.Changed != (tc.count == 0) || len(f.adds) != 1-int(tc.count) || len(f.updates) != 0 {
				t.Fatalf("result = %+v, error = %v, adds = %d, updates = %d", result, err, len(f.adds), len(f.updates))
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
