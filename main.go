package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/mail"
	stdsmtp "net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"os/signal"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/responses"
	"github.com/emersion/go-imap/server"
	"github.com/emersion/go-message"
	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
)

// Main

// Config is loaded once before startup; serving code only reads this snapshot.
type Config struct {
	Domain, CertFile, KeyFile                              string
	SMTPAddr, SubmissionAddr, SMTPSAddr                    string
	POP3Addr, POP3SAddr, IMAPAddr, IMAPSAddr, HTTPAddr     string
	OutboundMode                                           string
	ShowQueue, ShowVersion                                 bool
	QueueRetry                                             time.Duration
	RelayAddr, RelayUser, RelayPassword, RelayPasswordFile string
	RelayTLS, RelayCAFile                                  string
	RelayRootCAs                                           *x509.CertPool
	S3                                                     S3Config
}

var config Config

// Injected by Make or GoReleaser with -ldflags -X.
var version, commit, releaseTime string

func defaultConfig() Config {
	return Config{
		Domain: "t12e.cc", CertFile: "cert.pem", KeyFile: "key.pem",
		SMTPAddr: "127.0.0.1:2525", SubmissionAddr: "127.0.0.1:1587", SMTPSAddr: "127.0.0.1:1465",
		POP3Addr: "127.0.0.1:1110", POP3SAddr: "127.0.0.1:1995",
		IMAPAddr: "127.0.0.1:1143", IMAPSAddr: "127.0.0.1:1993", HTTPAddr: "127.0.0.1:8080",
		QueueRetry: time.Minute,
		S3:         S3Config{Region: "us-east-1"},
	}
}

func loadConfig(args []string, lookupEnv func(string) string) (Config, error) {
	getenv := func(key string) string { return lookupEnv("FMA_" + key) }
	c := defaultConfig()
	// All environment reads belong here. Flags override environment defaults.
	c.S3.Endpoint = getenv("S3_ENDPOINT")
	c.S3.Bucket = getenv("S3_BUCKET")
	if region := getenv("S3_REGION"); region != "" {
		c.S3.Region = region
	}
	c.S3.AccessKey = getenv("S3_ACCESS_KEY_ID")
	c.S3.SecretKey = getenv("S3_SECRET_ACCESS_KEY")
	c.S3.SessionToken = getenv("S3_SESSION_TOKEN")
	c.OutboundMode = getenv("OUTBOUND_MODE")
	c.RelayAddr = getenv("RELAY_ADDR")
	c.RelayUser = getenv("RELAY_USER")
	c.RelayPassword = getenv("RELAY_PASSWORD")
	c.RelayPasswordFile = getenv("RELAY_PASSWORD_FILE")
	c.RelayTLS = getenv("RELAY_TLS")
	c.RelayCAFile = getenv("RELAY_CA_FILE")
	f := flag.NewFlagSet("fma", flag.ContinueOnError)
	f.StringVar(&c.Domain, "domain", c.Domain, "local email domain")
	f.StringVar(&c.S3.Endpoint, "s3-endpoint", c.S3.Endpoint, "S3 endpoint URL; empty for AWS")
	f.StringVar(&c.S3.Bucket, "s3-bucket", c.S3.Bucket, "existing S3 bucket for all persistent data")
	f.StringVar(&c.S3.Region, "s3-region", c.S3.Region, "S3 region")
	f.StringVar(&c.CertFile, "cert", c.CertFile, "TLS certificate chain object key in the S3 bucket")
	f.StringVar(&c.KeyFile, "key", c.KeyFile, "TLS private key object key in the S3 bucket")
	f.StringVar(&c.SMTPAddr, "smtp", c.SMTPAddr, "inbound SMTP")
	f.StringVar(&c.SubmissionAddr, "submission", c.SubmissionAddr, "submission STARTTLS")
	f.StringVar(&c.SMTPSAddr, "smtps", c.SMTPSAddr, "submission TLS")
	f.StringVar(&c.POP3Addr, "pop3", c.POP3Addr, "POP3 STLS backend")
	f.StringVar(&c.POP3SAddr, "pop3s", c.POP3SAddr, "POP3S backend")
	f.StringVar(&c.IMAPAddr, "imap", c.IMAPAddr, "IMAP STARTTLS backend")
	f.StringVar(&c.IMAPSAddr, "imaps", c.IMAPSAddr, "IMAPS backend")
	f.StringVar(&c.HTTPAddr, "http", c.HTTPAddr, "HTTP health backend")
	f.StringVar(&c.OutboundMode, "outbound", c.OutboundMode, "disabled, relay or direct")
	f.BoolVar(&c.ShowVersion, "version", false, "print version and exit")
	f.BoolVar(&c.ShowQueue, "queue", false, "show outbound status without mail bodies")
	f.DurationVar(&c.QueueRetry, "queue-retry", c.QueueRetry, "initial outbound retry delay")
	if err := f.Parse(args); err != nil {
		return Config{}, err
	}
	// Queue inspection does not need an outbound transport.
	if c.ShowQueue || c.ShowVersion {
		return c, nil
	}
	if c.QueueRetry <= 0 {
		return Config{}, fmt.Errorf("queue-retry must be positive")
	}
	switch c.OutboundMode {
	case "", "disabled", "direct":
	case "relay":
		if _, _, err := net.SplitHostPort(c.RelayAddr); err != nil {
			return Config{}, fmt.Errorf("FMA_RELAY_ADDR must be host:port: %w", err)
		}
		if c.RelayUser == "" {
			return Config{}, fmt.Errorf("FMA_RELAY_USER is required")
		}
		if c.RelayTLS == "" {
			c.RelayTLS = "starttls"
		}
		if c.RelayTLS != "starttls" && c.RelayTLS != "implicit" {
			return Config{}, fmt.Errorf("FMA_RELAY_TLS must be starttls or implicit")
		}
		if c.RelayPassword == "" && c.RelayPasswordFile == "" {
			return Config{}, fmt.Errorf("relay password is required")
		}
	default:
		return Config{}, fmt.Errorf("outbound must be disabled, direct or relay")
	}
	return c, nil
}

func main() {
	var err error
	config, err = loadConfig(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	if err = run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if config.ShowVersion {
		fmt.Printf("fma %s\ncommit: %s\nrelease-time: %s\n", version, commit, releaseTime)
		return nil
	}
	bucket, err := connectBucket(config.S3)
	if err != nil {
		return err
	}
	if config.ShowQueue {
		objects = bucket
		return listQueue()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	guard := &guardedStore{base: bucket}
	objects = guard
	defer guard.close()

	if err := loadRelayObjects(&config); err != nil {
		return err
	}
	certPEM, err := objects.Get(config.CertFile)
	if err != nil {
		return fmt.Errorf("read TLS certificate from S3: %w", err)
	}
	keyPEM, err := objects.Get(config.KeyFile)
	if err != nil {
		return fmt.Errorf("read TLS key from S3: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	var listeners []net.Listener
	var closers []io.Closer
	shutdown := func() {
		for _, c := range closers {
			c.Close()
		}
	}
	defer shutdown()
	for i, addr := range []string{config.SMTPAddr, config.SubmissionAddr, config.SMTPSAddr, config.POP3SAddr, config.IMAPSAddr, config.HTTPAddr, config.POP3Addr, config.IMAPAddr} {
		l, e := net.Listen("tcp", addr)
		if e != nil {
			return e
		}
		if i >= 2 && i <= 4 {
			l = tls.NewListener(l, cfg)
		}
		listeners = append(listeners, l)
		closers = append(closers, l)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 9)
	serveGroup := func(jobs ...func() error) {
		defer wg.Done()
		wg.Add(len(jobs))
		for _, job := range jobs {
			go func() { defer wg.Done(); errors <- job() }()
		}
	}
	var jobs []func() error
	for i := 0; i < 3; i++ {
		s := smtp.NewServer(smtpBackend{requireAuth: i != 0})
		s.Domain = "mail." + config.Domain
		s.TLSConfig = cfg
		s.MaxMessageBytes = 25 << 20
		s.MaxRecipients = 100
		s.ReadTimeout = 5 * time.Minute
		s.WriteTimeout = time.Minute
		closers = append(closers, s)
		l := listeners[i]
		jobs = append(jobs, func() error { return s.Serve(l) })
	}
	im := server.New(imapBackend{})
	im.Enable(mailboxExtension{})
	im.TLSConfig = cfg
	im.MaxLiteralSize = 25 << 20
	im.AutoLogout = 30 * time.Minute
	web := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "fma mail server: SMTP, POP3S and IMAPS")
	})}
	pop := &popServer{cfg: cfg, conns: make(map[net.Conn]bool)}
	closers = append(closers, im, web, pop)
	wg.Add(2)
	jobs = append(jobs, func() error { return serveQueue(ctx) })
	go serveGroup(jobs...)
	go serveGroup(func() error { return pop.Serve(listeners[3]) }, func() error { return im.Serve(listeners[4]) }, func() error { return web.Serve(listeners[5]) }, func() error { return pop.Serve(listeners[6]) }, func() error { return im.Serve(listeners[7]) })
	log.Print("SMTP, submission, POP3/STLS, POP3S, IMAP/STARTTLS, IMAPS and HTTP backends ready")
	select {
	case <-ctx.Done():
		err = nil
	case err = <-errors:
	}
	cancel()
	shutdown()
	wg.Wait()
	return err
}

// S3 is the only persistent store. Credentials come from the startup snapshot;
// no shared credentials files, local cache, database or spool are opened.
type S3Config struct {
	Endpoint, Bucket, Region, AccessKey, SecretKey, SessionToken string
}
type objectStore interface {
	Get(string) ([]byte, error)
	Put(string, []byte) error
	Create(string, []byte) error
	Delete(string) error
	List(string) ([]string, error)
}

var objects objectStore

// Protocol libraries can return from Close before an in-flight backend call
// finishes. Drain storage calls and forbid new ones before releasing the lock.
type guardedStore struct {
	base   objectStore
	mu     sync.RWMutex
	closed bool
	lease  *bucketLease
}

func (g *guardedStore) close() { g.mu.Lock(); g.closed = true; g.mu.Unlock() }
func (g *guardedStore) Get(k string) ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.lease != nil && !g.lease.valid(time.Now()) {
		return nil, net.ErrClosed
	}
	return g.base.Get(k)
}
func (g *guardedStore) Put(k string, b []byte) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.lease != nil && !g.lease.valid(time.Now()) {
		return net.ErrClosed
	}
	return g.base.Put(k, b)
}
func (g *guardedStore) Create(k string, b []byte) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.lease != nil && !g.lease.valid(time.Now()) {
		return net.ErrClosed
	}
	return g.base.Create(k, b)
}
func (g *guardedStore) Delete(k string) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.lease != nil && !g.lease.valid(time.Now()) {
		return net.ErrClosed
	}
	return g.base.Delete(k)
}
func (g *guardedStore) List(k string) ([]string, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.lease != nil && !g.lease.valid(time.Now()) {
		return nil, net.ErrClosed
	}
	return g.base.List(k)
}

type s3Bucket struct {
	client   *s3.Client
	bucket   string
	deadline func() time.Time
}

func connectBucket(c S3Config) (*s3Bucket, error) {
	if c.Bucket == "" || strings.ContainsAny(c.Bucket, "/\\") {
		return nil, fmt.Errorf("s3-bucket must name an existing bucket")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("s3-endpoint must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil, fmt.Errorf("FMA_S3_ACCESS_KEY_ID and FMA_S3_SECRET_ACCESS_KEY are required (any values for Fals3y)")
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	client := s3.NewFromConfig(aws.Config{
		Region:           c.Region,
		Credentials:      credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, c.SessionToken),
		HTTPClient:       &http.Client{Timeout: 30 * time.Second},
		RetryMaxAttempts: 2,
	}, func(o *s3.Options) {
		o.UsePathStyle = true
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &c.Bucket}); err != nil {
		return nil, fmt.Errorf("connect S3 bucket %q: %w", c.Bucket, err)
	}
	return &s3Bucket{client: client, bucket: c.Bucket}, nil
}
func objectError(key string, err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey":
			return fmt.Errorf("object %q: %w", key, fs.ErrNotExist)
		case "PreconditionFailed", "ConditionalRequestConflict":
			return fmt.Errorf("object %q: %w", key, fs.ErrExist)
		}
	}
	return fmt.Errorf("S3 object %q: %w", key, err)
}
func (b *s3Bucket) requestContext() (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(5 * time.Second)
	if b.deadline != nil {
		if leaseEnd := b.deadline(); leaseEnd.Before(deadline) {
			deadline = leaseEnd
		}
	}
	return context.WithDeadline(context.Background(), deadline)
}
func (b *s3Bucket) Get(key string) ([]byte, error) {
	data, _, err := b.GetVersion(key)
	return data, err
}
func (b *s3Bucket) GetVersion(key string) ([]byte, string, error) {
	ctx, cancel := b.requestContext()
	defer cancel()
	result, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &b.bucket, Key: &key})
	if err != nil {
		return nil, "", objectError(key, err)
	}
	defer result.Body.Close()
	data, err := io.ReadAll(result.Body)
	return data, aws.ToString(result.ETag), objectError(key, err)
}
func (b *s3Bucket) Swap(key string, data []byte, etag string) error {
	if etag == "" {
		return fmt.Errorf("missing ETag for conditional write")
	}
	ctx, cancel := b.requestContext()
	defer cancel()
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &b.bucket, Key: &key, Body: bytes.NewReader(data), IfMatch: &etag})
	return objectError(key, err)
}
func (b *s3Bucket) put(key string, data []byte, create bool) error {
	ctx, cancel := b.requestContext()
	defer cancel()
	in := &s3.PutObjectInput{Bucket: &b.bucket, Key: &key, Body: bytes.NewReader(data)}
	if create {
		in.IfNoneMatch = aws.String("*")
	}
	_, err := b.client.PutObject(ctx, in)
	return objectError(key, err)
}
func (b *s3Bucket) Put(key string, data []byte) error    { return b.put(key, data, false) }
func (b *s3Bucket) Create(key string, data []byte) error { return b.put(key, data, true) }
func (b *s3Bucket) Delete(key string) error {
	ctx, cancel := b.requestContext()
	defer cancel()
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b.bucket, Key: &key})
	return objectError(key, err)
}
func (b *s3Bucket) List(prefix string) ([]string, error) {
	ctx, cancel := b.requestContext()
	defer cancel()
	pages := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{Bucket: &b.bucket, Prefix: &prefix})
	var keys []string
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, objectError(prefix, err)
		}
		for _, obj := range page.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// The scanner lease is renewed every 15 seconds with one missed interval of
// grace. It does not gate protocol traffic or already claimed tasks.
const leaseTTL = 30 * time.Second
const leaseRenewEvery = 15 * time.Second

type versionedStore interface {
	GetVersion(string) ([]byte, string, error)
	Swap(string, []byte, string) error
}
type lockRecord struct {
	Owner     string    `json:"owner"`
	StartedAt time.Time `json:"started_at"`
	RenewedAt time.Time `json:"renewed_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
type bucketLease struct {
	store    objectStore
	versions versionedStore
	mu       sync.Mutex
	record   lockRecord
	lost     error
}

func readLock(store versionedStore) (lockRecord, string, error) {
	data, etag, err := store.GetVersion(".lock")
	if err != nil {
		return lockRecord{}, "", err
	}
	var record lockRecord
	if err = json.Unmarshal(data, &record); err != nil || record.Owner == "" || record.StartedAt.IsZero() || record.RenewedAt.IsZero() || record.ExpiresAt.IsZero() {
		return lockRecord{}, "", fmt.Errorf("invalid or legacy .lock; stop the old instance before removing it")
	}
	return record, etag, nil
}
func lockBucket(store objectStore, now time.Time) (*bucketLease, error) {
	versions, ok := store.(versionedStore)
	if !ok {
		return nil, fmt.Errorf("S3 backend requires ETags and conditional updates")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	record := lockRecord{Owner: hex.EncodeToString(id[:]), StartedAt: now.UTC(), RenewedAt: now.UTC(), ExpiresAt: now.Add(leaseTTL).UTC()}
	data, _ := json.Marshal(record)
	previous, etag, err := readLock(versions)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		err = store.Create(".lock", data)
	case err != nil:
		return nil, err
	case now.Before(previous.ExpiresAt):
		return nil, fmt.Errorf("scanner locked until %s: %w", previous.ExpiresAt.Format(time.RFC3339Nano), fs.ErrExist)
	default:
		err = versions.Swap(".lock", data, etag)
	}
	if err != nil {
		return nil, fmt.Errorf("acquire bucket lease: %w", err)
	}
	return &bucketLease{store: store, versions: versions, record: record}, nil
}
func (l *bucketLease) owner() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.record.Owner
}
func (l *bucketLease) deadline() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.record.ExpiresAt
}
func (l *bucketLease) valid(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lost == nil && now.Before(l.record.ExpiresAt)
}
func (l *bucketLease) failure() error { l.mu.Lock(); defer l.mu.Unlock(); return l.lost }
func (l *bucketLease) fail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost == nil {
		l.lost = err
	}
}
func (l *bucketLease) renew(now time.Time) error {
	if !l.valid(now) {
		return fmt.Errorf("bucket lease expired")
	}
	current, etag, err := readLock(l.versions)
	if err != nil {
		return err
	}
	l.mu.Lock()
	owner := l.record.Owner
	l.mu.Unlock()
	if current.Owner != owner || !now.Before(current.ExpiresAt) {
		return fmt.Errorf("bucket lease ownership lost")
	}
	current.RenewedAt = now.UTC()
	current.ExpiresAt = now.Add(leaseTTL).UTC()
	data, _ := json.Marshal(current)
	if err := l.versions.Swap(".lock", data, etag); err != nil {
		return err
	}
	l.mu.Lock()
	l.record = current
	l.mu.Unlock()
	return nil
}
func (l *bucketLease) release(now time.Time) {
	current, etag, err := readLock(l.versions)
	l.mu.Lock()
	owner := l.record.Owner
	l.mu.Unlock()
	if err != nil || current.Owner != owner {
		return
	}
	current.ExpiresAt = now.UTC()
	data, _ := json.Marshal(current)
	// An expired record remains so release cannot race a new owner via DELETE.
	if err := l.versions.Swap(".lock", data, etag); err != nil && !errors.Is(err, fs.ErrExist) {
		log.Printf("release bucket lease: %v", err)
	}
}
func (l *bucketLease) keepAlive(ctx context.Context, cancel context.CancelFunc) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		renew := time.NewTicker(leaseRenewEvery)
		defer renew.Stop()
		watchdog := time.NewTicker(100 * time.Millisecond)
		defer watchdog.Stop()
		var pending chan error
		defer func() {
			if pending != nil {
				<-pending
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case <-watchdog.C:
				if !l.valid(time.Now()) {
					l.fail(fmt.Errorf("bucket lease expired; stopping writer"))
					cancel()
					return
				}
			case <-renew.C:
				if pending == nil {
					pending = make(chan error, 1)
					result := pending
					go func() { result <- l.renew(time.Now()) }()
				}
			case err := <-pending:
				pending = nil
				if err != nil {
					l.fail(fmt.Errorf("bucket lease renewal failed: %w", err))
					cancel()
					return
				}
			}
		}
	}()
	return done
}
func (g *guardedStore) GetVersion(k string) ([]byte, string, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed {
		return nil, "", net.ErrClosed
	}
	return g.base.(versionedStore).GetVersion(k)
}
func (g *guardedStore) Swap(k string, b []byte, etag string) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed {
		return net.ErrClosed
	}
	return g.base.(versionedStore).Swap(k, b, etag)
}

func loadRelayObjects(c *Config) error {
	if c.OutboundMode != "relay" {
		return nil
	}
	if c.RelayPasswordFile != "" {
		data, err := objects.Get(c.RelayPasswordFile)
		if err != nil {
			return fmt.Errorf("read relay password object: %w", err)
		}
		c.RelayPassword = strings.TrimRight(string(data), "\r\n")
	}
	if c.RelayPassword == "" {
		return fmt.Errorf("relay password is required")
	}
	if c.RelayCAFile != "" {
		data, err := objects.Get(c.RelayCAFile)
		if err != nil {
			return fmt.Errorf("read relay CA object: %w", err)
		}
		c.RelayRootCAs = x509.NewCertPool()
		if !c.RelayRootCAs.AppendCertsFromPEM(data) {
			return fmt.Errorf("invalid relay CA object")
		}
	}
	return nil
}

// Store

var mu sync.Mutex
var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func localUser(address string) string {
	parts := strings.Split(strings.ToLower(address), "@")
	if len(parts) > 2 || len(parts) == 2 && parts[1] != config.Domain && parts[1] != "mail."+config.Domain {
		return ""
	}
	if !usernameRE.MatchString(parts[0]) {
		return ""
	}
	return parts[0]
}

// Account existence and credentials are defined solely by <user>/.password.
// Read on every authentication/RCPT: external provisioning takes effect at once.
func userPassword(user string) ([]byte, error) {
	if !usernameRE.MatchString(user) {
		return nil, fs.ErrNotExist
	}
	data, err := objects.Get(path.Join(user, ".password"))
	if err != nil {
		return nil, err
	}
	data = bytes.TrimRight(data, "\r\n")
	if len(data) == 0 || bytes.ContainsAny(data, "\r\n") {
		return nil, fs.ErrNotExist
	}
	return data, nil
}
func authenticate(u, p string) bool {
	if p == "" {
		return false
	}
	stored, err := userPassword(localUser(u))
	return err == nil && subtle.ConstantTimeCompare(stored, []byte(p)) == 1
}
func readJSON[T any](path string) (value T, err error) {
	b, err := objects.Get(path)
	if err == nil {
		err = json.Unmarshal(b, &value)
	}
	return
}
func writeJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return objects.Put(path, b)
}

// Retry read-modify-write conflicts across protocol nodes without local state.
func updateJSON[T any](key string, change func(*T) error) error {
	store := objects.(versionedStore)
	for attempt := 0; attempt < 64; attempt++ {
		data, etag, err := store.GetVersion(key)
		missing := errors.Is(err, fs.ErrNotExist)
		if err != nil && !missing {
			return err
		}
		var value T
		if !missing {
			if err := json.Unmarshal(data, &value); err != nil {
				return err
			}
		}
		if err := change(&value); err != nil {
			return err
		}
		updated, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if bytes.Equal(data, updated) {
			return nil
		}
		if missing {
			err = objects.Create(key, updated)
		} else {
			err = store.Swap(key, updated, etag)
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return fmt.Errorf("S3 update contention: %s", key)
}
func updateCatalog(user string, change func(map[string]folderMeta) error) error {
	return updateJSON(catalogPath(user), func(c *map[string]folderMeta) error {
		if *c == nil {
			*c = map[string]folderMeta{}
		}
		return change(*c)
	})
}

func readMail(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, (25<<20)+1))
	if err == nil && len(b) > 25<<20 {
		err = &smtp.SMTPError{Code: 552, Message: "message too large"}
	}
	return b, err
}

// List only immediate JSON children; S3 listings are recursive and paginated.
func eachJSON[T any](dir string, visit func(string, T, error) error) error {
	keys, err := objects.List(strings.TrimSuffix(dir, "/") + "/")
	if err != nil {
		return err
	}
	for _, key := range keys {
		if path.Dir(key) != dir || !strings.HasSuffix(key, ".json") {
			continue
		}
		value, err := readJSON[T](key)
		if err := visit(key, value, err); err != nil {
			return err
		}
	}
	return nil
}
func messagePath(key string, uid uint32) string {
	return path.Join(key, fmt.Sprintf("%010d.json", uid))
}

// Callers hold mu for a consistent mailbox snapshot.
func mailboxSnapshot(key string) (box memory.Mailbox, next uint32, err error) {
	box.Messages, err = messages(key)
	if err == nil {
		next, err = nextUID(key)
	}
	return
}
func removeMessage(key string, uid uint32) error {
	err := objects.Delete(messagePath(key, uid))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
func messages(key string) ([]*memory.Message, error) {
	var result []*memory.Message
	err := eachJSON(key, func(_ string, m *memory.Message, err error) error {
		if err == nil {
			result = append(result, m)
		}
		return err
	})
	return result, err
}
func deliver(recipients []string, body []byte) error {
	mu.Lock()
	defer mu.Unlock()
	for _, u := range recipients {
		if err := appendMessage(u, body, nil, time.Now(), false); err != nil {
			return err
		}
	}
	return nil
}
func deleteMessages(u string, ids []uint32) error {
	mu.Lock()
	defer mu.Unlock()
	for _, id := range ids {
		if err := removeMessage(u, id); err != nil {
			return err
		}
	}
	return nil
}

func nextUID(u string) (uint32, error) {
	next, err := readJSON[uint32](path.Join(u, "next"))
	if errors.Is(err, fs.ErrNotExist) {
		return 1, nil
	}
	return next, err
}

// Folders

var standardFolders = []string{"INBOX", "Sent", "Drafts", "Trash", "Junk", "Archive"}
var specialFolders = map[string]string{"Sent": `\Sent`, "Drafts": `\Drafts`, "Trash": `\Trash`, "Junk": `\Junk`, "Archive": `\Archive`}

type folderMeta struct {
	Subscribed bool
	Validity   uint32
	Key        string
}

func canonicalFolder(name string) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}
func catalogPath(user string) string { return path.Join(user, "folders") }

// A single catalog object makes RENAME atomic without copying message objects.
// Folder storage IDs never change on rename and are never reused on recreation.
func folderCatalog(user string) (map[string]folderMeta, error) {
	catalog, err := readJSON[map[string]folderMeta](catalogPath(user))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]folderMeta{}, nil
	}
	if err == nil && catalog == nil {
		return nil, fmt.Errorf("invalid folder catalog")
	}
	return catalog, err
}
func newFolderMeta(user, name string) (folderMeta, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return folderMeta{}, err
	}
	key := path.Join(user, ".folders", hex.EncodeToString(id[:]))
	validity := binary.BigEndian.Uint32(id[:4]) | 1
	if name == "INBOX" {
		key = user
		validity = 1
	}
	return folderMeta{Subscribed: true, Validity: validity, Key: key}, nil
}
func validFolderName(name string) bool {
	return name != "" && len(name) <= 512 && !strings.ContainsAny(name, "\r\n\x00")
}
func ensureFolders(user string) error {
	return updateCatalog(user, func(catalog map[string]folderMeta) error {
		for _, name := range standardFolders {
			if _, ok := catalog[name]; !ok {
				meta, err := newFolderMeta(user, name)
				if err != nil {
					return err
				}
				catalog[name] = meta
			}
		}
		return nil
	})
}

func getFolder(user, name string) (folderMeta, error) {
	catalog, err := folderCatalog(user)
	if err != nil {
		return folderMeta{}, err
	}
	meta, ok := catalog[canonicalFolder(name)]
	if !ok {
		return folderMeta{}, backend.ErrNoSuchMailbox
	}
	if meta.Key == "" || meta.Validity == 0 {
		return folderMeta{}, fmt.Errorf("invalid folder metadata")
	}
	return meta, nil
}
func (u *imapUser) CreateMailbox(name string) error {
	name = canonicalFolder(name)
	if !validFolderName(name) {
		return fmt.Errorf("invalid mailbox name")
	}
	mu.Lock()
	defer mu.Unlock()
	if err := ensureFolders(u.name); err != nil {
		return err
	}
	return updateCatalog(u.name, func(catalog map[string]folderMeta) error {
		if _, ok := catalog[name]; ok {
			return fmt.Errorf("mailbox already exists")
		}
		meta, err := newFolderMeta(u.name, name)
		if err != nil {
			return err
		}
		catalog[name] = meta
		return nil
	})
}

func (u *imapUser) DeleteMailbox(name string) error {
	name = canonicalFolder(name)
	if name == "INBOX" {
		return fmt.Errorf("cannot delete INBOX")
	}
	mu.Lock()
	defer mu.Unlock()
	var meta folderMeta
	if err := updateCatalog(u.name, func(catalog map[string]folderMeta) error {
		var ok bool
		meta, ok = catalog[name]
		if !ok {
			return backend.ErrNoSuchMailbox
		}
		delete(catalog, name)
		return nil
	}); err != nil {
		return err
	}
	keys, err := objects.List(meta.Key + "/")
	if err == nil {
		for _, key := range keys {
			if err = objects.Delete(key); err != nil {
				break
			}
		}
	}
	if err != nil {
		log.Printf("deleted folder cleanup user=%s name=%q: %v", u.name, name, err)
	}
	return nil
}
func (u *imapUser) RenameMailbox(old, new string) error {
	old = canonicalFolder(old)
	new = canonicalFolder(new)
	if old == "INBOX" || new == "INBOX" {
		return fmt.Errorf("cannot rename INBOX")
	}
	if !validFolderName(new) {
		return fmt.Errorf("invalid mailbox name")
	}
	mu.Lock()
	defer mu.Unlock()
	return updateCatalog(u.name, func(catalog map[string]folderMeta) error {
		meta, ok := catalog[old]
		if !ok {
			return backend.ErrNoSuchMailbox
		}
		if _, ok := catalog[new]; ok {
			return fmt.Errorf("mailbox already exists")
		}
		delete(catalog, old)
		catalog[new] = meta
		return nil
	})
}

// Selected handles follow a renamed folder by ID and reject deleted folders.
// Callers hold mu.
func (b *inbox) exists() error {
	catalog, err := folderCatalog(b.user)
	if err != nil {
		return err
	}
	for name, meta := range catalog {
		if meta.Key == b.meta.Key {
			b.name = name
			return nil
		}
	}
	return backend.ErrNoSuchMailbox
}
func (b *inbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	data, err := readMail(body)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	if err := b.exists(); err != nil {
		return err
	}
	return appendMessage(b.key(), data, flags, date, b.name == "Sent")
}
func appendMessage(key string, body []byte, flags []string, date time.Time, unique bool) error {
	if unique {
		existing, err := messages(key)
		if err != nil {
			return err
		}
		for _, m := range existing {
			if bytes.Equal(m.Body, body) {
				return nil
			}
		}
	}
	var next uint32
	if err := updateJSON(path.Join(key, "next"), func(counter *uint32) error {
		if *counter == 0 {
			*counter = 1
		}
		if *counter == ^uint32(0) {
			return fmt.Errorf("UID exhausted")
		}
		next = *counter
		*counter++
		return nil
	}); err != nil {
		return err
	}
	if date.IsZero() {
		date = time.Now()
	}
	data, err := json.Marshal(&memory.Message{Uid: next, Date: date, Size: uint32(len(body)), Flags: flags, Body: body})
	if err != nil {
		return err
	}
	return objects.Create(messagePath(key, next), data)
}
func saveSent(user string, body []byte) error {
	if user == "" {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	if err := ensureFolders(user); err != nil {
		return err
	}
	meta, err := getFolder(user, "Sent")
	if err != nil {
		return err
	}
	return appendMessage(meta.Key, body, []string{imap.SeenFlag}, time.Now(), true)
}

// Smtp

type smtpBackend struct{ requireAuth bool }
type smtpSession struct {
	requireAuth bool
	peer        string
	user        string
	from        string
	remote      []string
	recipients  []string
}

func (b smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	_, encrypted := c.TLSConnectionState()
	peer := c.Conn().RemoteAddr().String()
	log.Printf("SMTP session peer=%q hello=%q tls=%t submission=%t", peer, c.Hostname(), encrypted, b.requireAuth)
	return &smtpSession{requireAuth: b.requireAuth, peer: peer}, nil
}
func (s *smtpSession) AuthMechanisms() []string { return []string{sasl.Plain, sasl.Login} }
func (s *smtpSession) Auth(mechanism string) (sasl.Server, error) {
	if mechanism == sasl.Login {
		return &loginServer{authenticate: func(u, p string) error { return s.authenticate("", u, p) }}, nil
	}
	if mechanism != sasl.Plain {
		log.Printf("SMTP auth unsupported peer=%q mechanism=%q", s.peer, mechanism)
		return nil, smtp.ErrAuthUnsupported
	}
	return sasl.NewPlainServer(s.authenticate), nil
}
func (s *smtpSession) authenticate(identity, u, p string) error {
	if !authenticate(u, p) || identity != "" && localUser(identity) != localUser(u) {
		log.Printf("SMTP auth failed peer=%q user=%q", s.peer, localUser(u))
		return smtp.ErrAuthFailed
	}
	s.user = localUser(u)
	log.Printf("SMTP auth accepted peer=%q user=%q", s.peer, s.user)
	return nil
}
func (s *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	if s.requireAuth && s.user == "" {
		log.Printf("SMTP MAIL rejected peer=%q code=530 reason=authentication-required", s.peer)
		return &smtp.SMTPError{Code: 530, Message: "authentication required"}
	}
	if s.user != "" && localUser(from) != s.user {
		log.Printf("SMTP MAIL rejected peer=%q user=%q code=553 reason=sender-mismatch", s.peer, s.user)
		return &smtp.SMTPError{Code: 553, Message: "sender must match authenticated user"}
	}
	s.Reset()
	s.from = from
	return nil
}
func (s *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	mu.Lock()
	defer mu.Unlock()
	u := localUser(to)
	if u == "" {
		address, err := mail.ParseAddress(to)
		if err != nil || address.Address != to || !strings.Contains(to, "@") || strings.ContainsAny(to, "\r\n") {
			return &smtp.SMTPError{Code: 553, Message: "invalid recipient"}
		}
		target := strings.ToLower(to[strings.LastIndex(to, "@")+1:])
		if target == config.Domain || target == "mail."+config.Domain {
			return &smtp.SMTPError{Code: 550, Message: "unknown local recipient"}
		}
		if s.user == "" {
			return &smtp.SMTPError{Code: 550, Message: "authentication required for external delivery"}
		}
		if !outboundEnabled() {
			return &smtp.SMTPError{Code: 451, Message: "outbound transport not configured; administrator must configure SMTP relay"}
		}
		if !slices.Contains(s.remote, to) {
			s.remote = append(s.remote, to)
		}
		return nil
	}
	if _, err := userPassword(u); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &smtp.SMTPError{Code: 550, Message: "unknown local recipient"}
		}
		return &smtp.SMTPError{Code: 451, Message: "account storage unavailable"}
	}
	if !slices.Contains(s.recipients, u) {
		s.recipients = append(s.recipients, u)
	}
	return nil
}
func (s *smtpSession) Data(r io.Reader) error {
	b, err := readMail(r)
	if err != nil {
		return err
	}
	if err = queueMail(s.user, s.from, s.recipients, s.remote, b); err != nil {
		return fmt.Errorf("mail storage: %w", err)
	}
	log.Printf("SMTP DATA accepted peer=%q user=%q local=%d remote=%d", s.peer, s.user, len(s.recipients), len(s.remote))
	return nil
}
func (s *smtpSession) Reset()      { s.recipients = nil; s.remote = nil; s.from = "" }
func (*smtpSession) Logout() error { return nil }

// Auth Login

// loginServer supports both AUTH LOGIN and AUTH LOGIN <base64-username>.
// go-smtp enforces TLS before invoking the authentication mechanism.
type loginServer struct {
	step         int
	username     string
	authenticate func(string, string) error
}

func (s *loginServer) Next(response []byte) ([]byte, bool, error) {
	switch s.step {
	case 0:
		s.step = 1
		if response == nil {
			return []byte("Username:"), false, nil
		}
		fallthrough
	case 1:
		s.username = string(response)
		s.step = 2
		return []byte("Password:"), false, nil
	case 2:
		s.step = 3
		user := s.username
		s.username = ""
		return nil, true, s.authenticate(user, string(response))
	default:
		return nil, false, fmt.Errorf("LOGIN exchange already completed")
	}
}

// Pop3

var popLocks sync.Map

// Close waits for every POP transaction before releasing the bucket lock.
type popServer struct {
	cfg    *tls.Config
	mu     sync.Mutex
	conns  map[net.Conn]bool
	closed bool
	wg     sync.WaitGroup
}

func (p *popServer) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			c.Close()
			return net.ErrClosed
		}
		p.conns[c] = true
		p.wg.Add(1)
		p.mu.Unlock()
		go func() {
			defer p.wg.Done()
			popSession(c, p.cfg)
			p.mu.Lock()
			delete(p.conns, c)
			p.mu.Unlock()
		}()
	}
}
func (p *popServer) Close() error {
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}
func popSession(c net.Conn, cfg *tls.Config) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Minute))
	_, secure := c.(*tls.Conn)
	scan := func() *bufio.Scanner { r := bufio.NewScanner(c); r.Buffer(make([]byte, 512), 4096); return r }
	reader := scan()
	out := textproto.NewWriter(bufio.NewWriter(c))
	reply := func(s string, a ...interface{}) { out.PrintfLine(s, a...) }
	multiline := func(lines []string) {
		w := out.DotWriter()
		for _, s := range lines {
			fmt.Fprintln(w, s)
		}
		w.Close()
	}
	reply("+OK fma POP3 ready")
	var user string
	var msgs []*memory.Message
	var held *sync.Mutex
	deleted := map[int]bool{}
	defer func() {
		if held != nil {
			held.Unlock()
		}
	}()
	for {
		c.SetDeadline(time.Now().Add(10 * time.Minute))
		if !reader.Scan() {
			return
		}
		command, arg, _ := strings.Cut(reader.Text(), " ")
		command = strings.ToUpper(command)
		if command == "CAPA" {
			reply("+OK")
			caps := []string{"USER", "UIDL", "TOP"}
			if !secure {
				caps = append(caps, "STLS")
			}
			multiline(caps)
			continue
		}
		if command == "QUIT" {
			var ids []uint32
			for i := range deleted {
				ids = append(ids, msgs[i-1].Uid)
			}
			if held != nil {
				if err := deleteMessages(user, ids); err != nil {
					reply("-ERR update failed")
					return
				}
			}
			reply("+OK goodbye")
			return
		}
		if held == nil {
			switch command {
			case "USER":
				user = localUser(arg)
				reply("+OK send PASS")
			case "STLS":
				if secure || arg != "" {
					reply("-ERR invalid STLS")
					continue
				}
				reply("+OK begin TLS")
				encrypted := tls.Server(c, cfg)
				if err := encrypted.Handshake(); err != nil {
					return
				}
				c = encrypted
				secure = true
				user = ""
				reader = scan()
				out = textproto.NewWriter(bufio.NewWriter(c))
			case "PASS":
				if !secure {
					reply("-ERR TLS required")
					continue
				}
				if !authenticate(user, arg) {
					reply("-ERR authentication failed")
					continue
				}
				v, _ := popLocks.LoadOrStore(user, &sync.Mutex{})
				lock := v.(*sync.Mutex)
				if !lock.TryLock() {
					reply("-ERR maildrop locked")
					continue
				}
				mu.Lock()
				snapshot, _, err := mailboxSnapshot(user)
				msgs = snapshot.Messages
				mu.Unlock()
				if err != nil {
					lock.Unlock()
					reply("-ERR storage unavailable")
					continue
				}
				held = lock
				reply("+OK authenticated")
			default:
				reply("-ERR authenticate first")
			}
			continue
		}
		fields := strings.Fields(arg)
		n, parseErr := strconv.Atoi(arg)
		if command == "TOP" && len(fields) == 2 {
			n, parseErr = strconv.Atoi(fields[0])
		}
		indexed := command == "DELE" || command == "RETR" || command == "TOP" || (command == "LIST" || command == "UIDL") && arg != ""
		if indexed && (parseErr != nil || n < 1 || n > len(msgs) || deleted[n]) {
			reply("-ERR no such message")
			continue
		}
		count, size := 0, 0
		for i, m := range msgs {
			if !deleted[i+1] {
				count++
				size += len(m.Body)
			}
		}
		switch command {
		case "STAT":
			reply("+OK %d %d", count, size)
		case "NOOP":
			reply("+OK")
		case "RSET":
			deleted = map[int]bool{}
			reply("+OK")
		case "LIST", "UIDL":
			line := func(i int) string {
				if command == "UIDL" {
					return fmt.Sprintf("%d %d", i, msgs[i-1].Uid)
				}
				return fmt.Sprintf("%d %d", i, len(msgs[i-1].Body))
			}
			if arg != "" {
				reply("+OK %s", line(n))
				continue
			}
			reply("+OK %d messages", count)
			var lines []string
			for i := range msgs {
				if !deleted[i+1] {
					lines = append(lines, line(i+1))
				}
			}
			multiline(lines)
		case "DELE":
			deleted[n] = true
			reply("+OK")
		case "RETR", "TOP":
			body := strings.ReplaceAll(string(msgs[n-1].Body), "\r\n", "\n")
			if command == "TOP" {
				if len(fields) != 2 {
					reply("-ERR TOP requires message and line count")
					continue
				}
				limit, err := strconv.Atoi(fields[1])
				if err != nil || limit < 0 {
					reply("-ERR invalid line count")
					continue
				}
				header, rest, _ := strings.Cut(body, "\n\n")
				lines := strings.Split(strings.TrimSuffix(rest, "\n"), "\n")
				if limit < len(lines) {
					lines = lines[:limit]
				}
				body = header + "\n\n" + strings.Join(lines, "\n")
			}
			reply("+OK")
			w := out.DotWriter()
			fmt.Fprint(w, body)
			w.Close()
		default:
			reply("-ERR unsupported command")
		}
	}
}

// Imap

type imapBackend struct{}
type imapUser struct{ name string }
type inbox struct {
	memory.Mailbox
	user string
	next uint32
	name string
	meta folderMeta
	conn server.Conn
}

func (imapBackend) Login(_ *imap.ConnInfo, u, p string) (backend.User, error) {
	if !authenticate(u, p) {
		return nil, backend.ErrInvalidCredentials
	}
	return &imapUser{localUser(u)}, nil
}
func (u *imapUser) Username() string { return u.name }
func (u *imapUser) Logout() error    { return nil }
func (u *imapUser) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	mu.Lock()
	defer mu.Unlock()
	if err := ensureFolders(u.name); err != nil {
		return nil, err
	}
	catalog, err := folderCatalog(u.name)
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range catalog {
		names = append(names, name)
	}
	sort.Strings(names)
	var boxes []backend.Mailbox
	for _, name := range names {
		meta := catalog[name]
		if !subscribed || meta.Subscribed {
			boxes = append(boxes, &inbox{user: u.name, name: name, meta: meta})
		}
	}
	log.Printf("IMAP LIST user=%s folders=%d", u.name, len(boxes))
	return boxes, nil
}
func (u *imapUser) GetMailbox(name string) (backend.Mailbox, error) {
	name = canonicalFolder(name)
	mu.Lock()
	defer mu.Unlock()
	if err := ensureFolders(u.name); err != nil {
		return nil, err
	}
	meta, err := getFolder(u.name, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, backend.ErrNoSuchMailbox
	}
	if err != nil {
		return nil, err
	}
	b := &inbox{user: u.name, name: name, meta: meta}
	b.Mailbox, b.next, err = mailboxSnapshot(b.key())
	log.Printf("IMAP mailbox user=%s name=%q messages=%d", u.name, name, len(b.Messages))
	return b, err
}
func (b *inbox) key() string  { return b.meta.Key }
func (b *inbox) Name() string { return b.name }
func (b *inbox) Info() (*imap.MailboxInfo, error) {
	info := &imap.MailboxInfo{Name: b.name, Delimiter: "/"}
	if attribute := specialFolders[b.name]; attribute != "" {
		info.Attributes = []string{attribute}
	}
	return info, nil
}
func (b *inbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	s, err := b.Mailbox.Status(items)
	s.Name = b.name
	s.UidNext = b.next
	s.UidValidity = b.meta.Validity
	s.Flags = []string{imap.SeenFlag, imap.AnsweredFlag, imap.FlaggedFlag, imap.DeletedFlag, imap.DraftFlag}
	s.PermanentFlags = append(append([]string(nil), s.Flags...), "\\*")
	for _, m := range b.Messages {
		if !slices.Contains(m.Flags, imap.SeenFlag) {
			s.Unseen++
		}
	}
	return s, err
}
func (b *inbox) SetSubscribed(value bool) error {
	mu.Lock()
	defer mu.Unlock()
	if err := b.exists(); err != nil {
		return err
	}
	return updateCatalog(b.user, func(catalog map[string]folderMeta) error {
		for name, meta := range catalog {
			if meta.Key == b.meta.Key {
				meta.Subscribed = value
				catalog[name] = meta
				b.meta = meta
				return nil
			}
		}
		return backend.ErrNoSuchMailbox
	})
}

func (b *inbox) Check() error { return b.Poll() }

// Resolve IMAP's star to the largest sequence number or UID in this snapshot.
func (b *inbox) resolve(set *imap.SeqSet, uid bool) {
	if set == nil || len(b.Messages) == 0 {
		return
	}
	maximum := uint32(len(b.Messages))
	if uid {
		maximum = b.Messages[len(b.Messages)-1].Uid
	}
	for i := range set.Set {
		if set.Set[i].Start == 0 {
			set.Set[i].Start = maximum
		}
		if set.Set[i].Stop == 0 {
			set.Set[i].Stop = maximum
		}
		if set.Set[i].Start > set.Set[i].Stop {
			set.Set[i].Start, set.Set[i].Stop = set.Set[i].Stop, set.Set[i].Start
		}
	}
}
func (b *inbox) ListMessages(uid bool, set *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	b.resolve(set, uid)
	if b.conn == nil || !b.conn.Context().MailboxReadOnly {
		for _, item := range items {
			if item == imap.FetchRFC822 || item == imap.FetchRFC822Text || strings.HasPrefix(string(item), "BODY[") {
				if err := b.UpdateMessagesFlags(uid, set, imap.AddFlags, []string{imap.SeenFlag}); err != nil {
					close(ch)
					return err
				}
				break
			}
		}
	}
	return b.Mailbox.ListMessages(uid, set, items, ch)
}
func (b *inbox) resolveSearch(c *imap.SearchCriteria) {
	b.resolve(c.SeqNum, false)
	b.resolve(c.Uid, true)
	for _, child := range c.Not {
		b.resolveSearch(child)
	}
	for _, pair := range c.Or {
		b.resolveSearch(pair[0])
		b.resolveSearch(pair[1])
	}
}
func (b *inbox) SearchMessages(uid bool, criteria *imap.SearchCriteria) ([]uint32, error) {
	b.resolveSearch(criteria)
	var ids []uint32
	for i, m := range b.Messages {
		entity, err := message.Read(bytes.NewReader(m.Body))
		if entity == nil {
			return nil, err
		}
		match, err := backendutil.Match(entity, uint32(i+1), m.Uid, m.Date, m.Flags, criteria)
		if err != nil {
			return nil, err
		}
		if match {
			id := uint32(i + 1)
			if uid {
				id = m.Uid
			}
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Imap Mutations

func (b *inbox) selected(uid bool, set *imap.SeqSet) []*memory.Message {
	b.resolve(set, uid)
	var selected []*memory.Message
	for i, m := range b.Messages {
		id := uint32(i + 1)
		if uid {
			id = m.Uid
		}
		if set.Contains(id) {
			selected = append(selected, m)
		}
	}
	return selected
}
func (b *inbox) UpdateMessagesFlags(uid bool, set *imap.SeqSet, op imap.FlagsOp, flags []string) error {
	mu.Lock()
	defer mu.Unlock()
	if err := b.exists(); err != nil {
		return err
	}
	for _, m := range b.selected(uid, set) {
		path := messagePath(b.key(), m.Uid)
		err := updateJSON(path, func(current **memory.Message) error {
			if *current == nil {
				return fs.ErrNotExist
			}
			(*current).Flags = backendutil.UpdateFlags((*current).Flags, op, flags)
			m.Flags = (*current).Flags
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
func (b *inbox) CopyMessages(uid bool, set *imap.SeqSet, dest string) error {
	dest = canonicalFolder(dest)
	mu.Lock()
	defer mu.Unlock()
	if err := b.exists(); err != nil {
		return err
	}
	meta, err := getFolder(b.user, dest)
	if err != nil {
		return err
	}
	for _, m := range b.selected(uid, set) {
		if err := appendMessage(meta.Key, m.Body, m.Flags, m.Date, false); err != nil {
			return err
		}
	}
	return nil
}
func (b *inbox) Expunge() error {
	mu.Lock()
	defer mu.Unlock()
	if err := b.exists(); err != nil {
		return err
	}
	kept := make([]*memory.Message, 0, len(b.Messages))
	for _, m := range b.Messages {
		if slices.Contains(m.Flags, imap.DeletedFlag) {
			if err := removeMessage(b.key(), m.Uid); err != nil {
				return err
			}
		} else {
			kept = append(kept, m)
		}
	}
	b.Messages = kept
	return nil
}
func (b *inbox) Poll() error {
	mu.Lock()
	if err := b.exists(); err != nil {
		mu.Unlock()
		return err
	}
	snapshot, next, err := mailboxSnapshot(b.key())
	mu.Unlock()
	if err != nil {
		return err
	}
	msgs := snapshot.Messages
	current := map[uint32]bool{}
	for _, m := range msgs {
		current[m.Uid] = true
	}
	var removed []uint32
	for i := len(b.Messages) - 1; i >= 0; i-- {
		if !current[b.Messages[i].Uid] {
			removed = append(removed, uint32(i+1))
		}
	}
	changed := len(msgs) != len(b.Messages) || next != b.next
	b.Messages = msgs
	b.next = next
	if b.conn == nil {
		return nil
	}
	if len(removed) > 0 {
		ch := make(chan uint32, len(removed))
		for _, n := range removed {
			ch <- n
		}
		close(ch)
		if err := b.conn.WriteResp(&responses.Expunge{SeqNums: ch}); err != nil {
			return err
		}
	}
	if changed {
		status := imap.NewMailboxStatus(b.name, []imap.StatusItem{imap.StatusMessages})
		status.Messages = uint32(len(msgs))
		return b.conn.WriteResp(&responses.Select{Mailbox: status})
	}
	return nil
}

// Bind selected mailboxes to their connection so NOOP can refresh snapshots.
// Do not advertise IDLE until asynchronous change notifications are implemented.
type mailboxExtension struct{}

func (mailboxExtension) Capabilities(server.Conn) []string { return nil }
func (mailboxExtension) NewConn(c server.Conn) server.Conn { return &mailboxConn{c} }

type mailboxConn struct{ server.Conn }

func (c *mailboxConn) Capabilities() []string {
	return slices.DeleteFunc(slices.Clone(c.Conn.Capabilities()), func(cap string) bool { return cap == "IDLE" || cap == "MOVE" })
}
func (mailboxExtension) Command(name string) server.HandlerFactory {
	switch name {
	case "SELECT":
		return func() server.Handler { return &mailboxSelect{} }
	case "EXAMINE":
		return func() server.Handler { h := &mailboxSelect{}; h.ReadOnly = true; return h }
	}
	return nil
}

type mailboxSelect struct{ server.Select }

func (h *mailboxSelect) Handle(c server.Conn) error {
	err := h.Select.Handle(c)
	if b, ok := c.Context().Mailbox.(*inbox); ok {
		b.conn = c
	}
	return err
}

// Outbound

const outbox = ".outbox"

type outboundRecipient struct {
	Address  string
	State    string
	Attempts int
	Next     time.Time
	Error    string
}
type outboundJob struct {
	ID         string
	User       string
	From       string
	Body       []byte
	Created    time.Time
	Recipients []outboundRecipient
	Notified   bool
	Archived   bool
	Complete   bool
	Preclaim   *taskClaim `json:"preclaim,omitempty"`
}

func outboundEnabled() bool { return config.OutboundMode == "relay" || config.OutboundMode == "direct" }

func queueMail(user, from string, local, remote []string, body []byte) error {
	if len(remote) == 0 {
		if err := deliver(local, body); err != nil {
			return err
		}
		if err := saveSent(user, body); err != nil {
			log.Printf("Sent archive failed user=%s: %v", user, err)
		}
		return nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	job := &outboundJob{ID: fmt.Sprintf("%x", id), User: user, From: from, Body: body, Created: time.Now()}
	for _, address := range remote {
		job.Recipients = append(job.Recipients, outboundRecipient{Address: address, State: "pending"})
	}
	// Publish a complete queue object only after local delivery succeeds.
	if err := deliver(local, body); err != nil {
		return err
	}
	if err := saveSent(user, body); err != nil {
		return err
	}
	job.Archived = true
	if err := writeJSON(path.Join(outbox, job.ID+".json"), job); err != nil {
		return err
	}
	log.Printf("outbound queued id=%s recipients=%d", job.ID, len(remote))
	return nil
}

func listQueue() error {
	return eachJSON(outbox, func(_ string, job *outboundJob, err error) error {
		if err != nil {
			return err
		}
		for _, r := range job.Recipients {
			fmt.Printf("%s %s %s attempts=%d next=%s error=%q\n", job.ID, r.Address, r.State, r.Attempts, r.Next.Format(time.RFC3339), r.Error)
		}
		return nil
	})
}
func (claim *claimedTask) process(ctx context.Context) error {
	job := claim.job
	if !job.Archived {
		if err := saveSent(job.User, job.Body); err != nil {
			return err
		}
		job.Archived = true
		if err := claim.save(ctx); err != nil {
			return err
		}
	}
	for i := range job.Recipients {
		r := &job.Recipients[i]
		if r.State != "pending" || time.Now().Before(r.Next) {
			continue
		}
		err := sendRemote(ctx, job, r.Address)
		if ctx.Err() != nil {
			return nil
		}
		r.Attempts++
		r.Error = ""
		if err == nil {
			r.State = "delivered"
		} else {
			r.Error = err.Error()
			var response *textproto.Error
			if errors.As(err, &response) && response.Code >= 500 || time.Since(job.Created) >= 72*time.Hour {
				r.State = "failed"
			}
			delay := config.QueueRetry * time.Duration(1<<min(r.Attempts-1, 6))
			r.Next = time.Now().Add(min(delay, time.Hour))
		}
		if err := claim.save(ctx); err != nil {
			return err
		}
		log.Printf("outbound id=%s recipient=%s state=%s attempt=%d error=%q", job.ID, r.Address, r.State, r.Attempts, r.Error)
	}
	var failed []string
	pending := false
	for _, r := range job.Recipients {
		if r.State == "pending" {
			pending = true
		}
		if r.State == "failed" {
			failed = append(failed, r.Address+": "+r.Error)
		}
	}
	if !pending && len(failed) > 0 && !job.Notified {
		notice := fmt.Sprintf("From: mailer-daemon@%s\r\nTo: %s\r\nDate: %s\r\nMessage-ID: <%s-failure@mail.%s>\r\nSubject: Delivery failed [%s]\r\nContent-Type: text/plain; charset=utf-8\r\nAuto-Submitted: auto-replied\r\n\r\nOutbound delivery failed. Queue ID: %s\r\n%s\r\n", config.Domain, job.From, time.Now().Format(time.RFC1123Z), job.ID, config.Domain, job.ID, job.ID, strings.Join(failed, "\r\n"))
		if err := deliver([]string{job.User}, []byte(notice)); err != nil {
			return err
		}
		job.Notified = true
		if err := claim.save(ctx); err != nil {
			return err
		}
	}
	if !pending {
		job.Complete = true
	}
	job.Preclaim = nil
	if err := claim.save(ctx); err != nil {
		return err
	}
	if job.Complete {
		return objects.Delete(claim.key)
	}
	return nil
}

const taskScanEvery = 15 * time.Second
const preclaimTimeout = 20 * time.Second
const maxClaimedTasks = 1024

type taskClaim struct {
	Owner     string    `json:"owner"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
type claimedTask struct {
	key, etag string
	job       *outboundJob
	expires   time.Time
}

// Preclaim and payload share an object, making takeover and completion atomic.
// Complete is a terminal state: CAS it before DELETE, so even a store without
// conditional DELETE cannot delete a successor. A later scan retries cleanup.
func preclaimTask(key, owner string, now time.Time) (*claimedTask, error) {
	store := objects.(versionedStore)
	data, etag, err := store.GetVersion(key)
	if err != nil {
		return nil, err
	}
	var job outboundJob
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	if job.Complete {
		return nil, objects.Delete(key)
	}
	if job.Preclaim != nil && now.Before(job.Preclaim.ExpiresAt) {
		return nil, nil
	}
	due := !job.Archived
	pending := false
	for _, r := range job.Recipients {
		if r.State == "pending" {
			pending = true
			due = due || !now.Before(r.Next)
		}
	}
	if !due && pending {
		return nil, nil
	}
	job.Preclaim = &taskClaim{Owner: owner, StartedAt: now.UTC(), ExpiresAt: now.Add(preclaimTimeout).UTC()}
	claim := &claimedTask{key: key, etag: etag, job: &job, expires: job.Preclaim.ExpiresAt}
	if err := claim.save(context.Background()); err != nil {
		return nil, err
	}
	return claim, nil
}
func (c *claimedTask) save(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(c.expires) {
		return context.DeadlineExceeded
	}
	data, err := json.Marshal(c.job)
	if err != nil {
		return err
	}
	store := objects.(versionedStore)
	if err := store.Swap(c.key, data, c.etag); err != nil {
		return err
	}
	current, etag, err := store.GetVersion(c.key)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, data) {
		return fs.ErrExist
	}
	c.etag = etag
	return nil
}
func (c *claimedTask) execute(ctx context.Context) error {
	ctx, cancel := context.WithDeadline(ctx, c.expires)
	defer cancel()
	return c.process(ctx)
}

// Only the lease holder reaches this scan; tasks start immediately after CAS.
// Returning full asks the scheduler to yield ownership for the next interval.
func scanTasks(ctx context.Context, lease *bucketLease, slots chan struct{}, workers *sync.WaitGroup, execute func(*claimedTask) error) (bool, error) {
	if !lease.valid(time.Now()) {
		return false, fmt.Errorf("scanner lease expired")
	}
	keys, err := objects.List(outbox + "/")
	if err != nil {
		return false, err
	}
	claimed := 0
	for _, key := range keys {
		if ctx.Err() != nil || !lease.valid(time.Now()) {
			break
		}
		if path.Dir(key) != outbox || !strings.HasSuffix(key, ".json") {
			continue
		}
		if len(slots) == cap(slots) || claimed == maxClaimedTasks {
			return true, nil
		}
		claim, err := preclaimTask(key, lease.owner(), time.Now())
		if err != nil && !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("preclaim %s: %v", key, err)
		}
		if claim == nil {
			continue
		}
		claimed++
		slots <- struct{}{}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-slots }()
			if err := execute(claim); err != nil {
				log.Printf("task %s: %v", claim.key, err)
			}
		}()
	}
	return claimed == maxClaimedTasks || len(slots) == cap(slots), nil
}

func serveQueue(ctx context.Context) error {
	ticker := time.NewTicker(taskScanEvery)
	defer ticker.Stop()
	var workers sync.WaitGroup
	slots := make(chan struct{}, maxClaimedTasks)
	var lease *bucketLease
	var stop context.CancelFunc
	var done <-chan struct{}
	release := func() {
		if lease == nil {
			return
		}
		stop()
		<-done
		lease.release(time.Now())
		lease = nil
	}
	defer func() { release(); workers.Wait() }()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if outboundEnabled() {
			if lease != nil && !lease.valid(time.Now()) {
				release()
			}
			if lease == nil && len(slots) < maxClaimedTasks {
				var err error
				lease, err = lockBucket(objects, time.Now())
				if err == nil {
					var leaseCtx context.Context
					leaseCtx, stop = context.WithCancel(ctx)
					done = lease.keepAlive(leaseCtx, stop)
				} else if !errors.Is(err, fs.ErrExist) {
					log.Printf("queue lease: %v", err)
				}
			}
			if lease != nil {
				full, err := scanTasks(ctx, lease, slots, &workers, func(c *claimedTask) error { return c.execute(ctx) })
				if err != nil {
					log.Printf("queue scan: %v", err)
				}
				if full || err != nil {
					release()
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Transport

func sendRemote(ctx context.Context, job *outboundJob, recipient string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if config.OutboundMode == "relay" {
		return sendSMTP(ctx, config.RelayAddr, job, recipient, true)
	}
	domain := recipient[strings.LastIndex(recipient, "@")+1:]
	records, err := net.DefaultResolver.LookupMX(ctx, domain)
	if err != nil {
		var dns *net.DNSError
		if !errors.As(err, &dns) || !dns.IsNotFound {
			return err
		}
		if _, err = net.DefaultResolver.LookupHost(ctx, domain); err != nil {
			return fmt.Errorf("recipient domain lookup failed: %w", err)
		}
	}
	if len(records) == 0 {
		records = []*net.MX{{Host: domain}}
	}
	var last error
	for _, mx := range records {
		if mx.Host == "." {
			return &textproto.Error{Code: 556, Msg: "recipient domain does not accept email (null MX)"}
		}
		last = sendSMTP(ctx, net.JoinHostPort(strings.TrimSuffix(mx.Host, "."), "25"), job, recipient, false)
		var response *textproto.Error
		if last == nil || errors.As(last, &response) && response.Code >= 500 {
			return last
		}
	}
	return last
}
func sendSMTP(ctx context.Context, address string, job *outboundJob, recipient string, relay bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if relay {
		cfg.RootCAs = config.RelayRootCAs
	}
	conn, err := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	raw := conn
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	implicit := relay && config.RelayTLS == "implicit"
	if implicit {
		encrypted := tls.Client(conn, cfg)
		if err = encrypted.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = encrypted
	}
	client, err := stdsmtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()
	if err = client.Hello("mail." + config.Domain); err != nil {
		return err
	}
	if !implicit {
		if err = client.StartTLS(cfg); err != nil {
			return fmt.Errorf("TLS upgrade failed: %w", err)
		}
	}
	if relay {
		if err = client.Auth(stdsmtp.PlainAuth("", config.RelayUser, config.RelayPassword, host)); err != nil {
			return fmt.Errorf("relay authentication failed: %w", err)
		}
	}
	if err = client.Mail(job.From); err != nil {
		return err
	}
	if err = client.Rcpt(recipient); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = bytes.NewReader(job.Body).WriteTo(writer); err != nil {
		return err
	}
	// Only the final DATA response confirms acceptance; QUIT failure must not resend it.
	if err = writer.Close(); err != nil {
		return err
	}
	return nil
}
