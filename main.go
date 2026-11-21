package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	updateURL  = "https://www.dy.fi/nic/update"
	webBaseURL = "https://www.dy.fi/"
	addFormURL = "https://www.dy.fi/nic/add_form"
	userAgent  = "dyfi-ip-update/2.0"

	ipCheckEvery   = 30 * time.Second // how often we detect outbound IP changes
	refreshPeriod  = 5 * 24 * time.Hour
	refreshJitter  = 4 * time.Hour   // dy.fi asks for "slightly random interval"
	requestSpacing = 3 * time.Second // pacing between API calls (avoid 'abuse')
	transientRetry = 10 * time.Minute
	maxAttempts    = 3
)

// Hostnames whose A records should always point to this machine's
// outbound IP. Used as fallback when no config file is present.
// Hosts on dy.fi expire after 7 days; the refresh loop renews them
// every ~5 days and re-registers them automatically if an update ever
// comes back as "nohost" (reservation expired/released).
var defaultDomains = []string{
	"jelinden.fi",
	"www.jelinden.fi",
	"incomewithdividends.dy.fi",
	"portfolio.jelinden.fi",
	"www.uutispuro.fi",
	"uutispuro.fi",
	"content-service.jelinden.fi",
}

// domainsToBeUpdated is set at startup from the JSON config file
// (see loadConfig), falling back to defaultDomains.
var domainsToBeUpdated []string

// Config is the on-disk JSON configuration, e.g. dyfi.json:
//
//	{ "domains": ["jelinden.fi", "www.jelinden.fi"] }
type Config struct {
	Domains []string `json:"domains"`
}

// loadConfig reads the JSON config at path. If the file does not
// exist, it returns the built-in defaults. A config with an empty or
// missing "domains" list is an error to avoid silently going idle.
func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		log.Printf("config %s not found, using built-in default domains", path)
		return &Config{Domains: defaultDomains}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	seen := map[string]bool{}
	domains := make([]string, 0, len(c.Domains))
	for _, d := range c.Domains {
		d = strings.TrimSpace(strings.ToLower(d))
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		domains = append(domains, d)
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("config %s contains no valid domains", path)
	}
	c.Domains = domains
	return &c, nil
}

var (
	dyUsername, dyPassword string
	client                 *http.Client

	ipMu       sync.Mutex
	outboundIP string

	jobs = make(chan job, 128)
)

type updateResult int

const (
	resultOK updateResult = iota
	resultAuthFail
	resultNoHost    // host not allocated to us -> needs re-registration
	resultTransient // dnserr / abuse / badip / unknown: worth retrying
)

func (r updateResult) String() string {
	switch r {
	case resultOK:
		return "ok"
	case resultAuthFail:
		return "auth-fail"
	case resultNoHost:
		return "no-host"
	default:
		return "transient"
	}
}

type job struct {
	host    string
	reason  string
	attempt int
}

func main() {
	log.SetFlags(log.LstdFlags)

	configPath := flag.String("config", "dyfi.json", "path to JSON config file with the domains to update")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	domainsToBeUpdated = cfg.Domains
	log.Printf("updating %d domains from %s: %v", len(domainsToBeUpdated), *configPath, domainsToBeUpdated)

	dyUsername = os.Getenv("dyUsername")
	dyPassword = os.Getenv("dyPassword")
	if dyUsername == "" || dyPassword == "" {
		log.Fatal("dyUsername and dyPassword environment variables must be set")
	}

	c, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client = newHTTPClient()
	if ip := GetOutboundIP(); ip != nil {
		setOutboundIP(ip)
		log.Println("outbound IP:", getOutboundIP())
	}

	// Initial update of all hosts (queued through the worker so that
	// request pacing applies).
	for _, d := range domainsToBeUpdated {
		jobs <- job{host: d, reason: "startup"}
	}

	go worker(c)
	go ipChangeWatcher(c)
	for _, d := range domainsToBeUpdated {
		go refreshLoop(c, d)
	}

	<-c.Done()
	log.Println("shutting down")
}

func newHTTPClient() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Fatalf("cookie jar: %v", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		Transport: &headerTransport{
			next:      transport,
			userAgent: userAgent,
		},
	}
}

// headerTransport adds the headers the dy.fi API example asks for.
type headerTransport struct {
	next      http.RoundTripper
	userAgent string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("User-Agent", t.userAgent)
	r.Header.Set("Cache-Control", "no-cache")
	return t.next.RoundTrip(r)
}

func worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-jobs:
			handleJob(ctx, j)
			select {
			case <-ctx.Done():
				return
			case <-time.After(requestSpacing):
			}
		}
	}
}

func handleJob(ctx context.Context, j job) {
	res, msg := updateIP(j.host)
	switch res {
	case resultOK:
		log.Printf("%s (%s): %s", j.host, j.reason, msg)

	case resultAuthFail:
		log.Printf("%s (%s): authentication FAILED (%s) - check dyUsername/dyPassword; not retrying",
			j.host, j.reason, msg)

	case resultNoHost:
		log.Printf("%s (%s): host not allocated to account (%s) - attempting re-registration",
			j.host, j.reason, msg)
		if err := reRegister(j.host); err != nil {
			log.Printf("%s: re-registration failed: %v", j.host, err)
			requeue(ctx, j, "after failed re-register")
			return
		}
		// Confirm the re-registration worked by updating again.
		res2, msg2 := updateIP(j.host)
		if res2 == resultOK {
			log.Printf("%s: re-registered and updated successfully: %s", j.host, msg2)
		} else {
			log.Printf("%s: still failing after re-register (%v): %s", j.host, res2, msg2)
			requeue(ctx, j, "still failing after re-register")
		}

	default: // resultTransient
		log.Printf("%s (%s): transient failure: %s", j.host, j.reason, msg)
		requeue(ctx, j, "transient failure")
	}
}

func requeue(ctx context.Context, j job, why string) {
	j.attempt++
	if j.attempt > maxAttempts {
		log.Printf("%s: giving up after %d attempts (%s); refresh loop will retry in ~%d days",
			j.host, j.attempt-1, why, int(refreshPeriod.Hours()/24))
		return
	}
	time.AfterFunc(transientRetry, func() {
		select {
		case jobs <- j:
		case <-ctx.Done():
		}
	})
}

// updateIP sends a dy.fi nic/update request for hostname and interprets
// the reply per https://www.dy.fi/page/specification
func updateIP(hostname string) (updateResult, string) {
	q := url.Values{}
	q.Set("hostname", hostname)
	req, err := http.NewRequest("GET", updateURL+"?"+q.Encode(), nil)
	if err != nil {
		return resultTransient, fmt.Sprintf("building request: %v", err)
	}
	req.SetBasicAuth(dyUsername, dyPassword)

	resp, err := client.Do(req)
	if err != nil {
		return resultTransient, fmt.Sprintf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return resultTransient, fmt.Sprintf("reading response: %v", err)
	}

	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(body)), "\n", 2)[0])
	return classifyUpdateResponse(resp.StatusCode, line)
}

// classifyUpdateResponse maps a dy.fi nic/update reply to a result, per
// https://www.dy.fi/page/specification
func classifyUpdateResponse(statusCode int, line string) (updateResult, string) {
	switch {
	case statusCode == http.StatusUnauthorized, strings.Contains(line, "badauth"):
		return resultAuthFail, line
	case strings.HasPrefix(line, "good"), strings.HasPrefix(line, "nochg"):
		return resultOK, line
	case strings.Contains(line, "nohost"), strings.Contains(line, "notfqdn"):
		return resultNoHost, line
	case strings.Contains(line, "dnserr"), strings.Contains(line, "abuse"),
		strings.Contains(line, "badip"), statusCode >= 500:
		return resultTransient, line
	default:
		return resultTransient, fmt.Sprintf("unexpected response %d: %q", statusCode, line)
	}
}

// refreshLoop renews one host's dy.fi reservation every 5-6 days
// (reservations expire after 7 days). The per-host jitter keeps the
// update times slightly random, as the dy.fi specification requests.
func refreshLoop(ctx context.Context, host string) {
	timer := time.NewTicker(refreshPeriod + jitterFor(host))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			jobs <- job{host: host, reason: "scheduled refresh"}
		}
	}
}

func jitterFor(host string) time.Duration {
	h := fnv.New64a()
	h.Write([]byte(host))
	return time.Duration(h.Sum64() % uint64(refreshJitter))
}

func ipChangeWatcher(ctx context.Context) {
	ticker := time.NewTicker(ipCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ip := GetOutboundIP()
			if ip == nil || ip.To4() == nil {
				continue
			}
			if ip.String() != getOutboundIP() {
				log.Println("outbound IP changed:", getOutboundIP(), "->", ip.String())
				setOutboundIP(ip)
				for _, d := range domainsToBeUpdated {
					jobs <- job{host: d, reason: "IP change"}
				}
			}
		}
	}
}

func setOutboundIP(ip net.IP) {
	ipMu.Lock()
	outboundIP = ip.To4().String()
	ipMu.Unlock()
}

func getOutboundIP() string {
	ipMu.Lock()
	defer ipMu.Unlock()
	return outboundIP
}

// GetOutboundIP finds the outbound IPv4 address by opening a UDP socket
// towards a public address (no packets are actually sent).
func GetOutboundIP() net.IP {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		log.Println("detecting outbound IP:", err)
		return nil
	}
	defer conn.Close()
	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil
	}
	return localAddr.IP
}

// ---------------------------------------------------------------------------
// Re-registration via the dy.fi web UI (there is no API endpoint for adding
// hosts; when a reservation expires, nic/update answers "nohost").
// ---------------------------------------------------------------------------

func reRegister(host string) error {
	if err := webLogin(); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	body, err := getPage(addFormURL)
	if err != nil {
		return fmt.Errorf("fetching add form: %w", err)
	}
	action, fields, err := parseAddForm(body)
	if err != nil {
		return err
	}
	fields.Set("hostname", host)

	resp, err := client.PostForm(action, fields)
	if err != nil {
		return fmt.Errorf("submitting add form: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("add form returned %d: %s", resp.StatusCode, snippet(string(respBody)))
	}
	log.Printf("%s: add form submitted to %s", host, action)
	return nil
}

func webLogin() error {
	v := url.Values{
		"c":        {"login"},
		"email":    {dyUsername},
		"password": {dyPassword},
		"submit":   {"login"},
	}
	resp, err := client.PostForm(webBaseURL, v)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	// Verify we are logged in: the account page only shows these when
	// there is an active session.
	home, err := getPage(webBaseURL)
	if err != nil {
		return fmt.Errorf("verifying session: %w", err)
	}
	lower := strings.ToLower(home)
	if strings.Contains(lower, "change password") || strings.Contains(lower, "vaihda salasana") {
		return nil
	}
	return fmt.Errorf("login did not establish a session (check credentials); HTTP status %d", resp.StatusCode)
}

func getPage(u string) (string, error) {
	resp, err := client.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("GET %s returned %d", u, resp.StatusCode)
	}
	return string(body), nil
}

var (
	formRe  = regexp.MustCompile(`(?is)<form\b[^>]*>.*?</form>`)
	inputRe = regexp.MustCompile(`(?is)<input\b[^>]*>`)
)

// parseAddForm finds the host-add form on the dy.fi add page and returns
// the absolute action URL plus all form fields (hidden values included).
func parseAddForm(page string) (string, url.Values, error) {
	for _, formHTML := range formRe.FindAllString(page, -1) {
		if !strings.Contains(strings.ToLower(formHTML), `name="hostname"`) {
			continue
		}
		fields := url.Values{}
		found := false
		for _, tag := range inputRe.FindAllString(formHTML, -1) {
			name := htmlAttr(tag, "name")
			if name == "" {
				continue
			}
			if name == "hostname" {
				found = true
			}
			fields.Set(name, htmlAttr(tag, "value"))
		}
		if !found {
			continue
		}
		action := htmlAttr(formTagOf(formHTML), "action")
		if action == "" {
			action = addFormURL // form with no action posts back to its own URL
		}
		base, err := url.Parse(webBaseURL)
		if err != nil {
			return "", nil, err
		}
		ref, err := url.Parse(action)
		if err != nil {
			return "", nil, err
		}
		return base.ResolveReference(ref).String(), fields, nil
	}
	return "", nil, fmt.Errorf("could not find host-add form on %s (page changed?)", addFormURL)
}

func formTagOf(form string) string {
	if idx := strings.Index(form, ">"); idx > 0 {
		return form[:idx]
	}
	return form
}

var attrCache sync.Map // compiled per-attribute regexes

func htmlAttr(tag, attr string) string {
	re, ok := attrCache.Load(attr)
	if !ok {
		re, _ = attrCache.LoadOrStore(attr, regexp.MustCompile(
			`(?i)\b`+attr+`\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`))
	}
	m := re.(*regexp.Regexp).FindStringSubmatch(tag)
	for i := 1; i < len(m) && i < 4; i++ {
		if m[i] != "" {
			return m[i]
		}
	}
	return ""
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
