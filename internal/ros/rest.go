package ros

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"

	"ros-ddns/internal/config"
)

const (
	ipv4AddressProperties = ".id,address,interface,actual-interface,disabled,invalid"
	ipv6AddressProperties = ipv4AddressProperties + ",global,link-local,deprecated"
	listEntryProperties   = ".id,list,address,comment,timeout,dynamic,disabled"
)

// APIError describes a non-success response from RouterOS.
type APIError struct {
	StatusCode int
	Message    string
	Detail     string
}

func (e *APIError) Error() string {
	message := e.Message
	if message == "" {
		message = http.StatusText(e.StatusCode)
	}
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	text := fmt.Sprintf("RouterOS HTTP %d: %s", e.StatusCode, message)
	if e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden {
		text += " (check credentials and RouterOS rest-api/read/write policies)"
	}
	return text
}

type restClient struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

var _ Client = (*restClient)(nil)

func newRESTClient(cfg config.RouterOSConfig) (Client, error) {
	base, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse RouterOS URL: %w", err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("RouterOS URL must use http or https and include a host")
	}
	base = base.JoinPath("rest")
	base.RawQuery, base.Fragment, base.RawFragment = "", "", ""
	base.ForceQuery = false

	tlsConfig := &tls.Config{InsecureSkipVerify: cfg.TLS.InsecureSkipVerify}
	if !cfg.TLS.InsecureSkipVerify && cfg.TLS.CAFile != "" {
		pem, err := os.ReadFile(cfg.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read RouterOS TLS CA file %q: %w", cfg.TLS.CAFile, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("parse RouterOS TLS CA file %q: no valid PEM certificates", cfg.TLS.CAFile)
		}
		tlsConfig.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &restClient{
		baseURL:  base.String(),
		username: cfg.Username,
		password: cfg.Password,
		httpClient: &http.Client{
			Transport: &noRedirectTransport{transport},
			Timeout:   cfg.Timeout.Duration,
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return redirectAPIError(req.Response)
			},
		},
	}, nil
}

type noRedirectTransport struct{ *http.Transport }

func (t *noRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.Transport.RoundTrip(req)
	if err == nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// net/http parses Location before CheckRedirect. Reject at the transport
		// boundary as well so malformed locations cannot leak through parse errors.
		resp.Body.Close()
		return nil, redirectAPIError(resp)
	}
	return resp, err
}

func redirectAPIError(resp *http.Response) *APIError {
	location := "<missing>"
	if resp.Header.Get("Location") != "" {
		location = "<invalid>"
		if target, err := resp.Location(); err == nil && target.Opaque == "" && (target.Scheme == "http" || target.Scheme == "https") {
			// Location may contain credentials or signed query parameters. Never
			// include those, or the redirect response body, in an error.
			target.User = nil
			target.RawQuery, target.Fragment, target.RawFragment = "", "", ""
			target.ForceQuery = false
			location = target.String()
		}
	}
	return &APIError{StatusCode: resp.StatusCode, Message: "redirect refused", Detail: fmt.Sprintf("Location: %q", location)}
}

type addressDTO struct {
	ID              string `json:".id"`
	Address         string `json:"address"`
	Interface       string `json:"interface"`
	ActualInterface string `json:"actual-interface"`
	Disabled        string `json:"disabled"`
	Invalid         string `json:"invalid"`
	Global          string `json:"global"`
	LinkLocal       string `json:"link-local"`
	Deprecated      string `json:"deprecated"`
}

type listEntryDTO struct {
	ID       string `json:".id"`
	List     string `json:"list"`
	Address  string `json:"address"`
	Comment  string `json:"comment"`
	Timeout  string `json:"timeout"`
	Dynamic  string `json:"dynamic"`
	Disabled string `json:"disabled"`
}

func (c *restClient) Addresses(ctx context.Context, family Family) ([]Address, error) {
	root, err := familyRoot(family)
	if err != nil {
		return nil, err
	}
	properties := ipv4AddressProperties
	if family == IPv6 {
		properties = ipv6AddressProperties
	}
	var entries []addressDTO
	if err := c.request(ctx, http.MethodGet, root+"/address", url.Values{".proplist": {properties}}, nil, &entries); err != nil {
		return nil, err
	}
	addresses := make([]Address, 0, len(entries))
	for _, entry := range entries {
		prefix, err := netip.ParsePrefix(entry.Address)
		if err != nil {
			return nil, fmt.Errorf("parse RouterOS %s address %q (id %q): %w", family, entry.Address, entry.ID, err)
		}
		addresses = append(addresses, Address{
			Interface:       entry.Interface,
			ActualInterface: entry.ActualInterface,
			Prefix:          prefix,
			Disabled:        routerOSBool(entry.Disabled),
			Invalid:         routerOSBool(entry.Invalid),
			Global:          routerOSBool(entry.Global),
			LinkLocal:       routerOSBool(entry.LinkLocal),
			Deprecated:      routerOSBool(entry.Deprecated),
		})
	}
	return addresses, nil
}

func (c *restClient) ListEntries(ctx context.Context, family Family, list string) ([]ListEntry, error) {
	root, err := familyRoot(family)
	if err != nil {
		return nil, err
	}
	var entries []listEntryDTO
	query := url.Values{"list": {list}, ".proplist": {listEntryProperties}}
	if err := c.request(ctx, http.MethodGet, root+"/firewall/address-list", query, nil, &entries); err != nil {
		return nil, err
	}
	result := make([]ListEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, ListEntry{
			ID:       entry.ID,
			List:     entry.List,
			Address:  entry.Address,
			Comment:  entry.Comment,
			Timeout:  entry.Timeout,
			Dynamic:  routerOSBool(entry.Dynamic),
			Disabled: routerOSBool(entry.Disabled),
		})
	}
	return result, nil
}

func (c *restClient) AddEntry(ctx context.Context, family Family, entry ListEntry) error {
	root, err := familyRoot(family)
	if err != nil {
		return err
	}
	body := struct {
		List    string `json:"list"`
		Address string `json:"address"`
		Comment string `json:"comment"`
		Timeout string `json:"timeout,omitempty"`
	}{entry.List, entry.Address, entry.Comment, entry.Timeout}
	err = c.request(ctx, http.MethodPut, root+"/firewall/address-list", nil, body, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Message+" "+apiErr.Detail), "already have such entry") {
		return fmt.Errorf("%w: %w", ErrEntryExists, err)
	}
	return err
}

func (c *restClient) RemoveEntry(ctx context.Context, family Family, id string) error {
	root, err := familyRoot(family)
	if err != nil {
		return err
	}
	// RouterOS resource IDs look like *29A4E. A literal * is valid in a URL path
	// segment, but PathEscape turns it into %2A, which RouterOS rejects as an
	// invalid resource identifier. Keep * literal and escape all other unsafe
	// characters in the ID.
	escapedID := strings.ReplaceAll(url.PathEscape(id), "%2A", "*")
	err = c.request(ctx, http.MethodDelete, root+"/firewall/address-list/"+escapedID, nil, nil, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func familyRoot(family Family) (string, error) {
	switch family {
	case IPv4:
		return "ip", nil
	case IPv6:
		return "ipv6", nil
	default:
		return "", fmt.Errorf("unsupported RouterOS address family %q", family)
	}
}

func routerOSBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes":
		return true
	default:
		return false
	}
}

func (c *restClient) request(ctx context.Context, method, resource string, query url.Values, body, result any) error {
	endpoint := c.baseURL + "/" + resource
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode RouterOS %s %s request: %w", method, resource, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("create RouterOS %s %s request: %w", method, resource, err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("User-Agent", "ros-ddns/1")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if resp != nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		// CheckRedirect errors are wrapped in a url.Error containing the raw
		// Location. Return only our sanitized error, also for 3xx without Location.
		return fmt.Errorf("RouterOS %s %s: %w", method, resource, redirectAPIError(resp))
	}
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode >= 300 && apiErr.StatusCode < 400 {
			err = apiErr // Discard url.Error's potentially credential-bearing URL.
		}
		return fmt.Errorf("RouterOS %s %s: %w", method, resource, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("RouterOS %s %s: %w", method, resource, decodeAPIError(resp))
	}
	if result == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("decode RouterOS %s %s response: %w", method, resource, err)
	}
	return nil
}

func decodeAPIError(resp *http.Response) *APIError {
	apiErr := &APIError{StatusCode: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		apiErr.Detail = fmt.Sprintf("read error response: %v", err)
		return apiErr
	}
	var body struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		apiErr.Detail = strings.TrimSpace(string(data))
		return apiErr
	}
	if body.Message != "" {
		apiErr.Message = body.Message
	}
	apiErr.Detail = body.Detail
	return apiErr
}
