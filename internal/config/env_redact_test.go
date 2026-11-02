package config

import (
	"errors"
	"fmt"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRedactEnvValues(t *testing.T) {
	for _, tc := range []struct {
		name, message, want string
		values              []string
	}{
		{"no values", "parse YAML: invalid value", "parse YAML: invalid value", nil},
		{"absent values", `log.level: unsupported value "verbose"`, `log.level: unsupported value "verbose"`, []string{"", "credential-secret", "some-verbose-password"}},
		{"empty message", "", "", []string{"secret"}},
		{"eight bytes anywhere", "xabcdefghy", "x[redacted]y", []string{"abcdefgh"}},
		{"seven bytes bounded", "abcdefg xabcdefgy (abcdefg)", "[redacted] xabcdefgy ([redacted])", []string{"abcdefg"}},
		{"short token boundaries", "p parse ip http p1 1p p_p p-p (p)", "[redacted] parse ip http p1 1p [redacted]_[redacted] [redacted]-[redacted] ([redacted])", []string{"p"}},
		{"ASCII boundaries", "épx 中p文 ApA 2p2", "épx 中[redacted]文 ApA 2p2", []string{"p"}},
		{"byte length", "x密码密y", "x[redacted]y", []string{"密码密"}},
		{"longest first", "private-value-extra private-value", "[redacted] [redacted]", []string{"private-value", "private-value-extra"}},
		{"overlapping values", "abcdefghijk", "[redacted]", []string{"abcdefgh", "defghijk"}},
		{"self overlap", "aaaaaaaaaaaa", "[redacted]", []string{"aaaaaaaa"}},
		{"short overlap", "a-b-c", "[redacted]", []string{"a-b", "b-c"}},
		{"adjacent values", "abcdefghijklmnop", "[redacted]", []string{"abcdefgh", "ijklmnop"}},
		{"trim and lowercase", "  SeCrEtX  |SeCrEtX|  secretx  |secretx", "[redacted]|[redacted]|[redacted]|[redacted]", []string{"  SeCrEtX  "}},
		{"domain normalization", `invalid domain "bad_domain"`, `invalid domain "[redacted]"`, []string{"  BAD_DOMAIN. \n"}},
		{"raw newline", "value: line one\nline two", "value: [redacted]", []string{"line one\nline two"}},
		{"decoder preview", "cannot unmarshal !!str `sensiti...` into bool", "cannot unmarshal !!str `[redacted]` into bool", []string{"sensitive-expanded-secret"}},
		{"no arbitrary prefix", "sensiti is a word", "sensiti is a word", []string{"sensitive-expanded-secret"}},
		{"quoted fragment", `invalid duration "123secretunit": unknown unit "secretunit" in duration "123secretunit"`, `invalid duration "[redacted]": unknown unit "[redacted]" in duration "[redacted]"`, []string{"123secretunit"}},
		{"marker preservation", "[redacted] redacted p", "[redacted] [redacted] [redacted]", []string{"redacted", "p"}},
		{"marker overlap", "[redacted]suffix", "[redacted][redacted]", []string{"[redacted]suffix"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := make(map[string]struct{})
			for _, value := range tc.values {
				values[value] = struct{}{}
			}
			got := redactEnvValues(tc.message, values)
			if got != tc.want {
				t.Fatalf("redactEnvValues = %q, want %q", got, tc.want)
			}
			if again := redactEnvValues(got, values); again != got {
				t.Errorf("redaction is not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestRedactEnvQuotedValues(t *testing.T) {
	for _, value := range []string{
		"p\nq", "s'ecret", `secret"value`, `secret\value`, "line one\nline two\r\n",
		"\tquotes \"double\" and 'single'\r\n", "\x01\x02\x03", "\xff\xfe\xfd", "\u200b",
	} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			values := map[string]struct{}{value: {}}
			message := fmt.Sprintf("invalid value %q", value)
			if got := redactEnvValues(message, values); got != `invalid value "[redacted]"` {
				t.Errorf("quoted value not redacted: %q", got)
			}
			if len(value) < 8 {
				quoted := strconv.Quote(value)
				message := "a" + quoted[1:len(quoted)-1] + "b"
				if got := redactEnvValues(message, values); got != message {
					t.Errorf("escaped short value lost its token boundaries: %q", got)
				}
			}
		})
	}
}

func TestRedactEnvError(t *testing.T) {
	values := map[string]struct{}{"private-value": {}}
	if err := redactEnvError(nil, values); err != nil {
		t.Errorf("nil error = %v", err)
	}
	unchanged := errors.New("ttl: must be between 1 and 86400")
	if got := redactEnvError(unchanged, values); got != unchanged {
		t.Error("unaffected error identity changed")
	}
	original := errors.New("invalid value: private-value")
	wrapped := fmt.Errorf("parse YAML: %w", original)
	redacted := redactEnvError(wrapped, values)
	if redacted.Error() != "parse YAML: invalid value: [redacted]" {
		t.Errorf("redacted error = %v", redacted)
	}
	if errors.Unwrap(redacted) != nil || errors.Is(redacted, original) {
		t.Error("redacted error retains an unsafe error chain")
	}
}

func TestExpandEnvScalarsTracksValues(t *testing.T) {
	t.Setenv("ROS_DDNS_TEST_SECRET", "private-value")
	t.Setenv("ROS_DDNS_TEST_SAME", "${ROS_DDNS_TEST_SAME}")
	t.Setenv("ROS_DDNS_TEST_EMPTY", "")
	const input = `# ${IGNORED_COMMENT}
key: &secret ${ROS_DDNS_TEST_SECRET}
alias: *secret
composite: prefix-${ROS_DDNS_TEST_SECRET}-suffix
same: ${ROS_DDNS_TEST_SAME}
empty: ${ROS_DDNS_TEST_EMPTY}
escaped: $${LITERAL}
plain: literal
`
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(input), &document); err != nil {
		t.Fatal(err)
	}
	changed, values, err := expandEnvScalars(&document)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 5 || len(values) != 5 {
		t.Fatalf("tracked %d nodes and %d values, want 5 of each", len(changed), len(values))
	}
	for _, value := range []string{"private-value", "prefix-private-value-suffix", "${ROS_DDNS_TEST_SAME}", "", "${LITERAL}"} {
		if _, ok := values[value]; !ok {
			t.Errorf("expanded scalar %q not tracked", value)
		}
	}
	for node := range changed {
		if node.Tag != "!!str" || node.Style != yaml.DoubleQuotedStyle {
			t.Errorf("expanded node lost string type or quoting: %+v", node)
		}
	}
}
