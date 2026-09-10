package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	stdsmtp "net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
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
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
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
	"github.com/migadu/go-pop3/pop3"
	"github.com/migadu/go-pop3/pop3server"
	jdescriptor "github.com/naust-mail/naust-jmap/core/descriptor"
	jmap "github.com/naust-mail/naust-jmap/core/jmap"
	jdb "github.com/naust-mail/naust-jmap/core/objectdb"
	jauth "github.com/naust-mail/naust-jmap/core/providers/auth"
	jbackend "github.com/naust-mail/naust-jmap/core/providers/backend"
	jblob "github.com/naust-mail/naust-jmap/core/providers/blob"
	jlease "github.com/naust-mail/naust-jmap/core/providers/lease"
	jruntime "github.com/naust-mail/naust-jmap/core/runtime"
	jmail "github.com/naust-mail/naust-jmap/datatypes/mail"
	jsearch "github.com/naust-mail/naust-jmap/datatypes/mail/search"
	jsubmit "github.com/naust-mail/naust-jmap/datatypes/mail/submit"
)

// Main

// Config is loaded once before startup; serving code only reads this snapshot.
type Config struct {
	Domain, CertFile, KeyFile, TLSDir, JMAPURL             string
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
var logger = newLogger(os.Stdout, os.Stderr, slog.LevelInfo)

// Keep ordinary output and diagnostics on separate streams without duplicating logs.
type logStreams struct{ output, diagnostics slog.Handler }

func (h logStreams) target(level slog.Level) slog.Handler {
	if level >= slog.LevelError {
		return h.diagnostics
	}
	return h.output
}
func (h logStreams) Enabled(ctx context.Context, level slog.Level) bool {
	return h.target(level).Enabled(ctx, level)
}
func (h logStreams) Handle(ctx context.Context, r slog.Record) error {
	return h.target(r.Level).Handle(ctx, r)
}
func (h logStreams) WithAttrs(attrs []slog.Attr) slog.Handler {
	return logStreams{h.output.WithAttrs(attrs), h.diagnostics.WithAttrs(attrs)}
}
func (h logStreams) WithGroup(name string) slog.Handler {
	return logStreams{h.output.WithGroup(name), h.diagnostics.WithGroup(name)}
}
func newLogger(output, diagnostics io.Writer, level slog.Level) *slog.Logger {
	options := &slog.HandlerOptions{Level: level}
	return slog.New(logStreams{slog.NewTextHandler(output, options), slog.NewTextHandler(diagnostics, options)})
}

func initLogger(value string) error {
	level := slog.LevelInfo
	if value != "" {
		if err := level.UnmarshalText([]byte(value)); err != nil {
			return fmt.Errorf("invalid LOG_LEVEL: %w", err)
		}
	}
	logger = newLogger(os.Stdout, os.Stderr, level)
	slog.SetDefault(logger)
	return nil
}

// Injected by Make or GoReleaser with -ldflags -X.
var version, commit, releaseTime string

func defaultConfig() Config {
	c, _ := loadConfig(nil, func(string) string { return "" })
	return c
}

func loadConfig(args []string, lookupEnv func(string) string) (Config, error) {
	getenv := func(key, fallback string) string {
		if value := lookupEnv("FMA_" + key); value != "" {
			return value
		}
		return fallback
	}
	c := Config{
		S3: S3Config{
			AccessKey: getenv("S3_ACCESS_KEY_ID", ""), SecretKey: getenv("S3_SECRET_ACCESS_KEY", ""),
			SessionToken: getenv("S3_SESSION_TOKEN", ""),
		},
		RelayAddr: getenv("RELAY_ADDR", ""), RelayUser: getenv("RELAY_USER", ""),
		RelayPassword: getenv("RELAY_PASSWORD", ""), RelayPasswordFile: getenv("RELAY_PASSWORD_FILE", ""),
		RelayTLS: getenv("RELAY_TLS", "starttls"), RelayCAFile: getenv("RELAY_CA_FILE", ""),
	}
	// Flags override environment values, which override the defaults below.
	f := flag.NewFlagSet("fma", flag.ContinueOnError)
	f.StringVar(&c.Domain, "domain", "t12e.cc", "local email domain")
	f.StringVar(&c.S3.Endpoint, "s3-endpoint", getenv("S3_ENDPOINT", ""), "S3 endpoint URL; empty for AWS")
	f.StringVar(&c.S3.Bucket, "s3-bucket", getenv("S3_BUCKET", ""), "existing S3 bucket for all persistent data")
	f.StringVar(&c.S3.Region, "s3-region", getenv("S3_REGION", "us-east-1"), "S3 region")
	f.StringVar(&c.CertFile, "cert", "cert.pem", "TLS certificate chain object key in the S3 bucket")
	f.StringVar(&c.KeyFile, "key", "key.pem", "TLS private key object key in the S3 bucket")
	f.StringVar(&c.TLSDir, "tls-dir", "", "read tls.crt and tls.key from a mounted TLS Secret directory instead of S3")
	f.StringVar(&c.SMTPAddr, "smtp", "127.0.0.1:2525", "inbound SMTP")
	f.StringVar(&c.SubmissionAddr, "submission", "127.0.0.1:1587", "submission STARTTLS")
	f.StringVar(&c.SMTPSAddr, "smtps", "127.0.0.1:1465", "submission TLS")
	f.StringVar(&c.POP3Addr, "pop3", "127.0.0.1:1110", "POP3 STLS backend")
	f.StringVar(&c.POP3SAddr, "pop3s", "127.0.0.1:1995", "POP3S backend")
	f.StringVar(&c.IMAPAddr, "imap", "127.0.0.1:1143", "IMAP STARTTLS backend")
	f.StringVar(&c.IMAPSAddr, "imaps", "127.0.0.1:1993", "IMAPS backend")
	f.StringVar(&c.JMAPURL, "jmap-url", getenv("JMAP_URL", ""), "public HTTPS origin for JMAP; defaults to https://mail.<domain>")
	f.StringVar(&c.HTTPAddr, "http", "127.0.0.1:8080", "HTTP JMAP API and health backend")
	f.StringVar(&c.OutboundMode, "outbound", getenv("OUTBOUND_MODE", ""), "disabled, relay or direct")
	f.BoolVar(&c.ShowVersion, "version", false, "print version and exit")
	f.BoolVar(&c.ShowQueue, "queue", false, "show outbound status without mail bodies")
	f.DurationVar(&c.QueueRetry, "queue-retry", time.Minute, "initial outbound retry delay")
	if err := f.Parse(args); err != nil {
		return Config{}, err
	}
	if f.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments")
	}
	return c, nil
}

// Validate the complete startup snapshot before opening S3 or any listener.
func checkConfig(c Config) error {
	if c.ShowVersion {
		return nil
	}
	if c.S3.Bucket == "" || strings.ContainsAny(c.S3.Bucket, "/\\ \t\r\n") {
		return fmt.Errorf("FMA_S3_BUCKET must name an existing bucket")
	}
	if c.S3.Region == "" {
		return fmt.Errorf("FMA_S3_REGION is required")
	}
	if c.S3.AccessKey == "" || c.S3.SecretKey == "" {
		return fmt.Errorf("FMA_S3_ACCESS_KEY_ID and FMA_S3_SECRET_ACCESS_KEY are required")
	}
	if c.S3.Endpoint != "" {
		u, err := url.Parse(c.S3.Endpoint)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("FMA_S3_ENDPOINT must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	// Queue inspection does not need an outbound transport.
	if c.ShowQueue {
		return nil
	}
	if c.Domain == "" || len(c.Domain) > 253 {
		return fmt.Errorf("domain is required and must be a DNS name")
	}
	for _, label := range strings.Split(c.Domain, ".") {
		if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(label) {
			return fmt.Errorf("invalid mail domain")
		}
	}
	if c.JMAPURL != "" {
		u, err := url.Parse(c.JMAPURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return fmt.Errorf("FMA_JMAP_URL must be a public HTTPS origin without credentials, path, query or fragment")
		}
	}
	if c.TLSDir == "" && (strings.TrimSpace(c.CertFile) == "" || strings.TrimSpace(c.KeyFile) == "") {
		return fmt.Errorf("cert and key S3 object keys are required")
	}
	seen := map[string]bool{}
	for _, addr := range []string{c.SMTPAddr, c.SubmissionAddr, c.SMTPSAddr, c.POP3Addr, c.POP3SAddr, c.IMAPAddr, c.IMAPSAddr, c.HTTPAddr} {
		if err := checkAddress(addr); err != nil {
			return err
		}
		_, port, _ := net.SplitHostPort(addr)
		if seen[addr] && port != "0" {
			return fmt.Errorf("duplicate listener address %q", addr)
		}
		seen[addr] = true
	}
	if c.QueueRetry <= 0 {
		return fmt.Errorf("queue-retry must be positive")
	}
	switch c.OutboundMode {
	case "", "disabled", "direct":
	case "relay":
		if err := checkAddress(c.RelayAddr); err != nil {
			return fmt.Errorf("FMA_RELAY_ADDR must be host:port: %w", err)
		}
		if c.RelayUser == "" {
			return fmt.Errorf("FMA_RELAY_USER is required")
		}
		if c.RelayTLS != "starttls" && c.RelayTLS != "implicit" {
			return fmt.Errorf("FMA_RELAY_TLS must be starttls or implicit")
		}
		if c.RelayPassword == "" && c.RelayPasswordFile == "" {
			return fmt.Errorf("relay password is required")
		}
	default:
		return fmt.Errorf("outbound must be disabled, direct or relay")
	}
	return nil
}

func checkAddress(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid host:port address %q: %w", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("invalid port in %q", addr)
	}
	return nil
}

func main() {
	if err := initLogger(os.Getenv("LOG_LEVEL")); err != nil {
		logger.Error("logging configuration failed", "error", err)
		os.Exit(1)
	}
	var err error
	config, err = loadConfig(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	if err = run(); err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if err := checkConfig(config); err != nil {
		return err
	}
	if !config.ShowVersion && !config.ShowQueue && (config.OutboundMode == "" || config.OutboundMode == "disabled") {
		logger.Warn("outbound delivery is disabled")
	}
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
	useJMAP = true
	defer guard.close()

	if err := loadRelayObjects(&config); err != nil {
		return err
	}
	cert, err := loadTLSCertificate(config)
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
		s.ErrorLog = slog.NewLogLogger(logger.Handler(), slog.LevelError)
		s.Domain = "mail." + config.Domain
		s.TLSConfig = cfg
		s.MaxMessageBytes = maxMailSize
		s.MaxRecipients = 100
		s.ReadTimeout = 5 * time.Minute
		s.WriteTimeout = time.Minute
		closers = append(closers, s)
		l := listeners[i]
		jobs = append(jobs, func() error { return s.Serve(l) })
	}
	im := server.New(imapBackend{})
	im.ErrorLog = slog.NewLogLogger(logger.Handler(), slog.LevelError)
	im.Enable(mailboxExtension{})
	im.TLSConfig = cfg
	im.MaxLiteralSize = 25 << 20
	im.AutoLogout = 30 * time.Minute
	web := &http.Server{ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError), ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(serveJMAP)}
	pop, pops := newPOPServer(cfg), newPOPServer(cfg)
	closers = append(closers, im, web, pop, pops)
	wg.Add(2)
	jobs = append(jobs, func() error { return serveQueue(ctx) })
	go serveGroup(jobs...)
	go serveGroup(func() error { return pops.Serve(listeners[3]) }, func() error { return im.Serve(listeners[4]) }, func() error { return web.Serve(listeners[5]) }, func() error { return pop.Serve(listeners[6]) }, func() error { return im.Serve(listeners[7]) })
	logger.Info("SMTP, submission, POP3/STLS, POP3S, IMAP/STARTTLS, IMAPS and HTTP backends ready")
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

// Mounted TLS Secrets are read-only input; mail state remains exclusively in S3.
func loadTLSCertificate(c Config) (tls.Certificate, error) {
	if c.TLSDir != "" {
		cert, err := tls.LoadX509KeyPair(filepath.Join(c.TLSDir, "tls.crt"), filepath.Join(c.TLSDir, "tls.key"))
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("read mounted TLS Secret: %w", err)
		}
		return cert, nil
	}
	certPEM, err := objects.Get(c.CertFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read TLS certificate from S3: %w", err)
	}
	keyPEM, err := objects.Get(c.KeyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read TLS key from S3: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
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
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	client := s3.NewFromConfig(aws.Config{
		Region:           c.Region,
		Credentials:      credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, c.SessionToken),
		HTTPClient:       &http.Client{Transport: transport},
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
		case "NoSuchKey", "NoSuchUpload":
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
		logger.Error(fmt.Sprintf("release bucket lease: %v", err))
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

type identityKind string

const (
	kindAccount identityKind = "account"
	kindAlias   identityKind = "alias"
	kindProxy   identityKind = "proxy"
)

func readKind(id string) (identityKind, error) {
	if !usernameRE.MatchString(id) {
		return "", fs.ErrNotExist
	}
	data, err := objects.Get(path.Join(id, ".kind"))
	if err != nil {
		return "", err
	}
	kind := identityKind(strings.TrimRight(string(data), "\r\n"))
	switch kind {
	case kindAccount, kindAlias, kindProxy:
		return kind, nil
	}
	return "", fs.ErrNotExist
}
func aliasTarget(id string) (string, error) {
	data, err := objects.Get(path.Join(id, ".alias"))
	if err != nil {
		return "", err
	}
	target := strings.TrimRight(string(data), "\r\n")
	if !usernameRE.MatchString(target) {
		return "", fs.ErrNotExist
	}
	return target, nil
}

// Aliases keep their own prefix; credentials and mailbox data belong to RootID.
type accountIdentity struct {
	LoginID string
	RootID  string
}

func resolveIdentity(address string) (accountIdentity, error) {
	id := localUser(address)
	identity := accountIdentity{LoginID: id, RootID: id}
	if id == "" {
		return identity, fs.ErrNotExist
	}
	seen := make(map[string]bool)
	for range 16 {
		if seen[identity.RootID] {
			return identity, fs.ErrNotExist
		}
		seen[identity.RootID] = true
		kind, err := readKind(identity.RootID)
		if err != nil {
			return identity, err
		}
		if kind == kindAccount {
			return identity, nil
		}
		if kind != kindAlias {
			return identity, fs.ErrNotExist
		}
		target, err := aliasTarget(identity.RootID)
		if err != nil {
			return identity, err
		}
		identity.RootID = target
	}
	return identity, fs.ErrNotExist
}
func accountPassword(address string) (accountIdentity, []byte, error) {
	identity, err := resolveIdentity(address)
	if err != nil {
		return identity, nil, err
	}
	password, err := userPassword(identity.RootID)
	return identity, password, err
}
func authenticateAccount(address, password string) (accountIdentity, error) {
	identity, stored, err := accountPassword(address)
	if err != nil {
		return identity, err
	}
	if password == "" || subtle.ConstantTimeCompare(stored, []byte(password)) != 1 {
		return identity, fs.ErrPermission
	}
	return identity, nil
}

// Read credentials on every login/RCPT; external S3 changes take effect at once.
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
	_, err := authenticateAccount(u, p)
	return err == nil
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
	if useJMAP {
		return jmapUpdateCatalog(user, change)
	}
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
		if path.Dir(key) != dir || strings.HasPrefix(path.Base(key), ".") || !strings.HasSuffix(key, ".json") {
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
	if useJMAP {
		return jmapRemoveMessage(key, uid)
	}
	err := objects.Delete(messagePath(key, uid))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
func legacyMessages(key string) ([]*memory.Message, error) {
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

func legacyNextUID(u string) (uint32, error) {
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
func legacyFolderCatalog(user string) (map[string]folderMeta, error) {
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
		logger.Error(fmt.Sprintf("deleted folder cleanup user=%s name=%q: %v", u.name, name, err))
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
	if useJMAP {
		ctx := context.Background()
		a, err := jmapForKey(b.key())
		if err != nil {
			return err
		}
		box, err := a.mailbox(ctx, b.key())
		if err != nil {
			return err
		}
		ref, err := storeMailStream(ctx, a.root, body)
		if err != nil {
			return err
		}
		_, err = a.importStoredMail(ctx, box, ref, flags, date, b.name == "Sent")
		return err
	}

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
	if useJMAP {
		a, err := jmapForKey(key)
		if err != nil {
			return err
		}
		ctx := context.Background()
		box, err := a.mailbox(ctx, key)
		if err != nil {
			return err
		}
		email, err := a.importMail(ctx, box, body, flags, date)
		if err == nil {
			_, err = a.uid(ctx, box, email)
		}
		return err
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
	loginID     string
	requireAuth bool
	peer        string
	user        string
	from        string
	remote      []string
	proxies     []proxyRoute
	recipients  []string
}

func (b smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	_, encrypted := c.TLSConnectionState()
	peer := c.Conn().RemoteAddr().String()
	logger.Debug(fmt.Sprintf("SMTP session peer=%q hello=%q tls=%t submission=%t", peer, c.Hostname(), encrypted, b.requireAuth))
	return &smtpSession{requireAuth: b.requireAuth, peer: peer}, nil
}
func (s *smtpSession) AuthMechanisms() []string { return []string{sasl.Plain, sasl.Login} }
func (s *smtpSession) Auth(mechanism string) (sasl.Server, error) {
	if mechanism == sasl.Login {
		return &loginServer{authenticate: func(u, p string) error { return s.authenticate("", u, p) }}, nil
	}
	if mechanism != sasl.Plain {
		logger.Warn(fmt.Sprintf("SMTP auth unsupported peer=%q mechanism=%q", s.peer, mechanism))
		return nil, smtp.ErrAuthUnsupported
	}
	return sasl.NewPlainServer(s.authenticate), nil
}
func (s *smtpSession) authenticate(identity, u, p string) error {
	account, err := authenticateAccount(u, p)
	if err == nil && identity != "" {
		authorized, e := resolveIdentity(identity)
		if e != nil || authorized.RootID != account.RootID {
			err = fs.ErrPermission
		}
	}
	if err != nil {
		logger.Error(fmt.Sprintf("SMTP auth failed peer=%q user=%q", s.peer, localUser(u)))
		return smtp.ErrAuthFailed
	}
	s.loginID, s.user = account.LoginID, account.RootID
	logger.Info(fmt.Sprintf("SMTP auth accepted peer=%q login=%q root=%q", s.peer, s.loginID, s.user))
	return nil
}
func (s *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	if s.requireAuth && s.user == "" {
		logger.Warn(fmt.Sprintf("SMTP MAIL rejected peer=%q code=530 reason=authentication-required", s.peer))
		return &smtp.SMTPError{Code: 530, Message: "authentication required"}
	}
	if s.user != "" {
		identity, err := resolveIdentity(from)
		if err != nil || identity.RootID != s.user {
			return &smtp.SMTPError{Code: 553, Message: "sender must belong to authenticated account"}
		}
	}
	s.Reset()
	s.from = from
	return nil
}

// A proxy-only prefix can receive mail without enabling password authentication.
// Routing is resolved once at RCPT and persisted with the accepted task.
type proxyRoute struct{ Address, Owner string }

func resolveDelivery(user string) (local, remote, owner string, err error) {
	seen := map[string]bool{}
	for range 16 {
		if seen[user] {
			return "", "", "", &smtp.SMTPError{Code: 550, Message: "mail routing loop"}
		}
		seen[user] = true
		kind, e := readKind(user)
		if e != nil {
			return "", "", "", e
		}
		switch kind {
		case kindAccount:
			if _, e := userPassword(user); e != nil {
				return "", "", "", e
			}
			return user, "", owner, nil
		case kindAlias:
			user, e = aliasTarget(user)
			if e != nil {
				return "", "", "", e
			}
		case kindProxy:
			data, e := objects.Get(path.Join(user, ".proxy"))
			if e != nil {
				return "", "", "", e
			}
			target := strings.TrimRight(string(data), "\r\n")
			addr, parseErr := mail.ParseAddress(target)
			if parseErr != nil || addr.Address != target || !strings.Contains(target, "@") || strings.ContainsAny(target, "\r\n") {
				return "", "", "", &smtp.SMTPError{Code: 550, Message: "invalid mail proxy target"}
			}
			if owner == "" {
				owner = user
			}
			next := localUser(target)
			if next == "" {
				domain := strings.ToLower(target[strings.LastIndex(target, "@")+1:])
				if domain == config.Domain || domain == "mail."+config.Domain {
					return "", "", "", fs.ErrNotExist
				}
				return "", target, owner, nil
			}
			user = next
		}
	}
	return "", "", "", &smtp.SMTPError{Code: 550, Message: "mail routing chain too long"}
}

func proxyBody(body []byte) ([]byte, error) {
	message, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil {
		return nil, &smtp.SMTPError{Code: 554, Message: "invalid forwarded message headers"}
	}
	hops := 0
	for _, value := range message.Header["X-Fma-Proxy-Hops"] {
		n, e := strconv.Atoi(value)
		if e != nil || n < 0 || n >= 16 {
			return nil, &smtp.SMTPError{Code: 554, Message: "mail proxy hop limit exceeded"}
		}
		hops = max(hops, n)
	}
	return append(fmt.Appendf(nil, "X-FMA-Proxy-Hops: %d\r\n", hops+1), body...), nil
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
	local, remote, owner, err := resolveDelivery(u)
	if err != nil {
		var response *smtp.SMTPError
		if errors.As(err, &response) {
			return response
		}
		if errors.Is(err, fs.ErrNotExist) {
			return &smtp.SMTPError{Code: 550, Message: "unknown local recipient"}
		}
		return &smtp.SMTPError{Code: 451, Message: "account storage unavailable"}
	}
	if remote != "" {
		if !outboundEnabled() {
			return &smtp.SMTPError{Code: 451, Message: "outbound transport not configured for proxy"}
		}
		if !slices.Contains(s.remote, remote) {
			s.remote = append(s.remote, remote)
		}
		route := proxyRoute{Address: remote, Owner: owner}
		if !slices.Contains(s.proxies, route) {
			s.proxies = append(s.proxies, route)
		}
	} else if !slices.Contains(s.recipients, local) {
		s.recipients = append(s.recipients, local)
	}
	return nil
}
func (s *smtpSession) Data(r io.Reader) error {
	if useJMAP {
		return s.streamData(r)
	}
	b, err := readMail(r)
	if err != nil {
		return err
	}
	if err = queueMail(s.user, s.from, s.recipients, s.remote, b, s.proxies...); err != nil {
		return fmt.Errorf("mail storage: %w", err)
	}
	logger.Info(fmt.Sprintf("SMTP DATA accepted peer=%q user=%q local=%d remote=%d", s.peer, s.user, len(s.recipients), len(s.remote)))
	return nil
}
func (s *smtpSession) Reset()      { s.recipients = nil; s.remote = nil; s.proxies = nil; s.from = "" }
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

// Each listener has its own library Server, including connection shutdown.
func newPOPServer(cfg *tls.Config) *pop3server.Server {
	return pop3server.New(pop3server.Options{Logger: logger,
		TLSConfig: cfg, Greeting: "fma POP3 ready", StrictSessionErrors: true,
		IdleTimeout: 10 * time.Minute, WriteTimeout: time.Minute, MaxLineLength: 4096,
		NewSession: func(*pop3server.Conn) (pop3server.Session, error) {
			return &popMailbox{deleted: make(map[int]bool)}, nil
		},
	})
}

type popMailbox struct {
	loginID string
	user    string
	msgs    []*memory.Message
	deleted map[int]bool
	held    *sync.Mutex
}

var _ pop3server.SessionSASL = (*popMailbox)(nil)

func (p *popMailbox) Login(ctx context.Context, user, password string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	account, err := authenticateAccount(user, password)
	if err != nil {
		return &pop3server.Error{Code: "AUTH", Message: "authentication failed"}
	}
	user = account.RootID
	v, _ := popLocks.LoadOrStore(user, &sync.Mutex{})
	lock := v.(*sync.Mutex)
	if !lock.TryLock() {
		return &pop3server.Error{Code: "IN-USE", Message: "maildrop locked"}
	}
	mu.Lock()
	snapshot, _, err := mailboxSnapshot(user)
	mu.Unlock()
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		lock.Unlock()
		return &pop3server.Error{Code: "SYS/TEMP", Message: "storage unavailable"}
	}
	p.loginID, p.user, p.msgs, p.held = account.LoginID, user, snapshot.Messages, lock
	return nil
}
func (p *popMailbox) Close() error {
	// Only QUIT commits deletes. Disconnect, timeout, and shutdown roll back.
	if p.held != nil {
		p.held.Unlock()
		p.held = nil
	}
	p.msgs = nil
	clear(p.deleted)
	return nil
}
func (p *popMailbox) AuthenticateMechanisms() []string { return []string{"PLAIN"} }
func (p *popMailbox) AuthenticatePlain(ctx context.Context, identity, user, password string) error {
	if identity != "" {
		a, err := resolveIdentity(identity)
		b, otherErr := resolveIdentity(user)
		if err != nil || otherErr != nil || a.RootID != b.RootID {
			return &pop3server.Error{Code: "AUTH", Message: "identity mismatch"}
		}
	}
	return p.Login(ctx, user, password)
}
func (p *popMailbox) message(ctx context.Context, n int) (*memory.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n < 1 || n > len(p.msgs) || p.deleted[n] {
		return nil, fmt.Errorf("no such message")
	}
	return p.msgs[n-1], nil
}
func (p *popMailbox) List(ctx context.Context, n int) ([]pop3.MessageInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n != 0 {
		m, err := p.message(ctx, n)
		if err != nil {
			return nil, err
		}
		return []pop3.MessageInfo{{Num: n, Size: int64(len(m.Body))}}, nil
	}
	var result []pop3.MessageInfo
	for i, m := range p.msgs {
		if !p.deleted[i+1] {
			result = append(result, pop3.MessageInfo{Num: i + 1, Size: int64(len(m.Body))})
		}
	}
	return result, nil
}
func (p *popMailbox) Stat(ctx context.Context) (int, int64, error) {
	messages, err := p.List(ctx, 0)
	var size int64
	for _, m := range messages {
		size += m.Size
	}
	return len(messages), size, err
}
func (p *popMailbox) Uidl(ctx context.Context, n int) ([]pop3.MessageUidl, error) {
	messages, err := p.List(ctx, n)
	var result []pop3.MessageUidl
	for _, m := range messages {
		result = append(result, pop3.MessageUidl{Num: m.Num, UniqueID: strconv.FormatUint(uint64(p.msgs[m.Num-1].Uid), 10)})
	}
	return result, err
}
func (p *popMailbox) Retr(ctx context.Context, n int) (io.ReadCloser, error) {
	m, err := p.message(ctx, n)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(m.Body)), nil
}
func (p *popMailbox) Top(ctx context.Context, n, lines int) (io.ReadCloser, error) {
	m, err := p.message(ctx, n)
	if err != nil {
		return nil, err
	}
	if lines < 0 {
		return nil, fmt.Errorf("invalid line count")
	}
	r := bufio.NewReader(bytes.NewReader(m.Body))
	var result bytes.Buffer
	inBody := false
	for !inBody || lines > 0 {
		line, err := r.ReadBytes('\n')
		result.Write(line)
		if inBody {
			lines--
		} else if len(bytes.TrimRight(line, "\r\n")) == 0 {
			inBody = true
		}
		if err != nil {
			break
		}
	}
	return io.NopCloser(bytes.NewReader(result.Bytes())), nil
}
func (p *popMailbox) Dele(ctx context.Context, n int) error {
	if _, err := p.message(ctx, n); err != nil {
		return err
	}
	p.deleted[n] = true
	return nil
}
func (p *popMailbox) Rset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clear(p.deleted)
	return nil
}
func (p *popMailbox) Noop(ctx context.Context) error { return ctx.Err() }
func (p *popMailbox) Quit(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var ids []uint32
	for i := range p.deleted {
		ids = append(ids, p.msgs[i-1].Uid)
	}
	if err := deleteMessages(p.user, ids); err != nil {
		return 0, &pop3server.Error{Code: "SYS/TEMP", Message: "update failed"}
	}
	clear(p.deleted)
	return len(ids), nil
}

// Imap

type imapBackend struct{}
type imapUser struct{ name, loginID string }
type inbox struct {
	memory.Mailbox
	user string
	next uint32
	name string
	meta folderMeta
	conn server.Conn
}

func (imapBackend) Login(_ *imap.ConnInfo, u, p string) (backend.User, error) {
	identity, err := authenticateAccount(u, p)
	if err != nil {
		return nil, backend.ErrInvalidCredentials
	}
	return &imapUser{name: identity.RootID, loginID: identity.LoginID}, nil
}
func (u *imapUser) Username() string {
	if u.loginID != "" {
		return u.loginID
	}
	return u.name
}
func (u *imapUser) Logout() error { return nil }
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
	logger.Debug(fmt.Sprintf("IMAP LIST user=%s folders=%d", u.name, len(boxes)))
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
	logger.Debug(fmt.Sprintf("IMAP mailbox user=%s name=%q messages=%d", u.name, name, len(b.Messages)))
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
		if useJMAP {
			updated, err := jmapUpdateFlags(b.key(), m.Uid, op, flags)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			m.Flags = updated
			continue
		}
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
	ProxyOwners []string `json:"proxy_owners,omitempty"`
	Address     string
	State       string
	Attempts    int
	Next        time.Time
	Error       string
}
type outboundJob struct {
	Blob       *jmapBlobRef `json:"blob,omitempty"`
	Prefix     string       `json:"prefix,omitempty"`
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

func queueMail(user, from string, local, remote []string, body []byte, proxies ...proxyRoute) error {
	if len(remote) == 0 {
		if err := deliver(local, body); err != nil {
			return err
		}
		if err := saveSent(user, body); err != nil {
			logger.Error(fmt.Sprintf("Sent archive failed user=%s: %v", user, err))
		}
		return nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	queuedBody := body
	if len(proxies) > 0 {
		var err error
		queuedBody, err = proxyBody(body)
		if err != nil {
			return err
		}
	}
	job := &outboundJob{ID: fmt.Sprintf("%x", id), User: user, From: from, Body: queuedBody, Created: time.Now()}
	for _, address := range remote {
		recipient := outboundRecipient{Address: address, State: "pending"}
		for _, route := range proxies {
			if route.Address == address && !slices.Contains(recipient.ProxyOwners, route.Owner) {
				recipient.ProxyOwners = append(recipient.ProxyOwners, route.Owner)
			}
		}
		job.Recipients = append(job.Recipients, recipient)
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
	logger.Info(fmt.Sprintf("outbound queued id=%s recipients=%d", job.ID, len(remote)))
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
		var err error
		if job.Blob != nil {
			err = deliverMailStream(ctx, []string{job.User}, job.Blob, true)
		} else {
			err = saveSent(job.User, job.Body)
		}
		if err != nil {
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
		logger.Info(fmt.Sprintf("outbound id=%s recipient=%s state=%s attempt=%d error=%q", job.ID, r.Address, r.State, r.Attempts, r.Error))
	}
	notices := map[string][]string{}
	proxyFailures := map[string][]string{}
	pending := false
	for _, r := range job.Recipients {
		if r.State == "pending" {
			pending = true
		}
		if r.State == "failed" {
			for _, owner := range r.ProxyOwners {
				proxyFailures[owner] = append(proxyFailures[owner], r.Address+": "+r.Error)
			}
			if job.User != "" {
				notices[job.User] = append(notices[job.User], r.Address+": "+r.Error)
			}
		}
	}
	if !pending && !job.Notified {
		for owner, failures := range proxyFailures {
			// A proxy has no mailbox. Retain diagnostics in S3 without keeping the message.
			report := struct {
				TaskID   string
				Created  time.Time
				Failures []string
			}{job.ID, job.Created, failures}
			if err := writeJSON(path.Join(owner, ".proxy-errors", job.ID+".json"), report); err != nil {
				return err
			}
		}
		for user, failed := range notices {
			notice := fmt.Sprintf("From: mailer-daemon@%s\r\nTo: %s@%s\r\nDate: %s\r\nMessage-ID: <%s-failure@mail.%s>\r\nSubject: Delivery failed [%s]\r\nContent-Type: text/plain; charset=utf-8\r\nAuto-Submitted: auto-replied\r\n\r\nOutbound delivery failed. Queue ID: %s\r\n%s\r\n", config.Domain, user, config.Domain, time.Now().Format(time.RFC1123Z), job.ID, config.Domain, job.ID, job.ID, strings.Join(failed, "\r\n"))
			// Failure notices stay local and never re-enter proxy routing.
			if err := deliver([]string{user}, []byte(notice)); err != nil {
				return err
			}
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
			logger.Error(fmt.Sprintf("preclaim %s: %v", key, err))
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
				logger.Error(fmt.Sprintf("task %s: %v", claim.key, err))
			}
		}()
	}
	if useJMAP {
		count, err := scanJMAPTasks(ctx, lease, min(maxClaimedTasks-claimed, cap(slots)-len(slots)))
		claimed += count
		if err != nil {
			return false, err
		}
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
		if outboundEnabled() || useJMAP {
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
					logger.Error(fmt.Sprintf("queue lease: %v", err))
				}
			}
			if lease != nil {
				full, err := scanTasks(ctx, lease, slots, &workers, func(c *claimedTask) error { return c.execute(ctx) })
				if err != nil {
					logger.Error(fmt.Sprintf("queue scan: %v", err))
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
	body, err := job.openBody(ctx)
	if err != nil {
		return err
	}
	defer body.Close()
	if _, err = io.Copy(writer, body); err != nil {
		return err
	}
	// Only the final DATA response confirms acceptance; QUIT failure must not resend it.
	if err = writer.Close(); err != nil {
		return err
	}
	return nil
}

// JMAP's ordered key/value records for one account commit in one conditional
// S3 write. Binary MIME blobs live separately, so transactions copy metadata only.
// There is no local database and no process-owned lease or background maintainer.
type jmapBackend struct {
	key    string
	store  objectStore
	mu     sync.RWMutex
	closed bool
}

func (b *jmapBackend) snapshot(ctx context.Context) (map[string][]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	data, etag, err := b.store.(versionedStore).GetVersion(b.key)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string][]byte{}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var state map[string][]byte
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, "", err
	}
	if state == nil {
		return nil, "", fmt.Errorf("invalid JMAP metadata: %s", b.key)
	}
	return state, etag, nil
}
func (b *jmapBackend) Get(ctx context.Context, key []byte) ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return nil, fs.ErrClosed
	}
	state, _, err := b.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	value, ok := state[hex.EncodeToString(key)]
	if !ok {
		return nil, jbackend.ErrNotFound
	}
	return value, nil
}
func (b *jmapBackend) Scan(ctx context.Context, start, end []byte, reverse bool, visit func([]byte, []byte) bool) error {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return fs.ErrClosed
	}
	state, _, err := b.snapshot(ctx)
	b.mu.RUnlock()
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(state))
	for key := range state {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if reverse {
		slices.Reverse(keys)
	}
	for _, encoded := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, err := hex.DecodeString(encoded)
		if err != nil {
			return err
		}
		if bytes.Compare(key, start) >= 0 && (end == nil || bytes.Compare(key, end) < 0) {
			if !visit(key, state[encoded]) {
				break
			}
		}
	}
	return nil
}
func (b *jmapBackend) WriteBatch(ctx context.Context, batch *jbackend.Batch) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return fs.ErrClosed
	}
	for _, op := range batch.Ops {
		if op.Kind == jbackend.OpSet && bytes.Contains(op.Key, []byte("EmailSubmission\x00\x01")) {
			root := strings.SplitN(b.key, "/", 2)[0]
			err := b.store.Create(".jmap-queue/"+string(jmapAccountID(root)), []byte(root))
			if err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			break
		}
	}
	for attempt := 0; attempt < 128; attempt++ {
		state, etag, err := b.snapshot(ctx)
		if err != nil {
			return err
		}
		// Assertions are tested against the pre-batch state, even when a Set
		// earlier in this batch targets the same key.
		for _, op := range batch.Ops {
			if op.Kind != jbackend.OpAssert {
				continue
			}
			value, found := state[hex.EncodeToString(op.Key)]
			if (op.Value == nil && found) || (op.Value != nil && (!found || !bytes.Equal(value, op.Value))) {
				return jbackend.ErrAssertFailed
			}
		}
		for _, op := range batch.Ops {
			key := hex.EncodeToString(op.Key)
			switch op.Kind {
			case jbackend.OpSet:
				value := bytes.Clone(op.Value)
				if bytes.Contains(op.Key, []byte("\x00\x01o\x00\x01Email\x00\x01")) {
					var email jdb.Object
					if json.Unmarshal(value, &email) == nil {
						uids := jvalue[map[jmap.Id]uint32](email, "fmaUIDs")
						boxes := jvalue[map[jmap.Id]bool](email, "mailboxIds")
						changed := false
						for box := range uids {
							if !boxes[box] {
								delete(uids, box)
								changed = true
							}
						}
						if changed {
							email["fmaUIDs"] = jraw(uids)
							value = jraw(email)
						}
					}
				}
				state[key] = value
			case jbackend.OpDelete:
				delete(state, key)
			case jbackend.OpAdd:
				var value int64
				if old, ok := state[key]; ok {
					value, err = jbackend.DecodeInt64(old)
					if err != nil {
						return err
					}
				}
				state[key] = jbackend.EncodeInt64(value + op.Delta)
			case jbackend.OpAssert:
			default:
				return fmt.Errorf("unknown JMAP batch operation: %d", op.Kind)
			}
		}
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if etag == "" {
			err = b.store.Create(b.key, data)
		} else {
			err = b.store.(versionedStore).Swap(b.key, data, etag)
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return fmt.Errorf("JMAP metadata contention: %s", b.key)
}
func (b *jmapBackend) Close() error { b.mu.Lock(); defer b.mu.Unlock(); b.closed = true; return nil }

type jmapBlobs struct{ store objectStore }

var jmapBlobIDRE = regexp.MustCompile(`^G[A-Za-z0-9_-]{43}$`)

func jmapAccountID(root string) jmap.Id { return jmap.Id("A" + hex.EncodeToString([]byte(root))) }
func jmapRoot(acct jmap.Id) (string, error) {
	if !strings.HasPrefix(string(acct), "A") {
		return "", jauth.ErrUnauthenticated
	}
	data, err := hex.DecodeString(string(acct)[1:])
	root := string(data)
	if err != nil || !usernameRE.MatchString(root) {
		return "", jauth.ErrUnauthenticated
	}
	return root, nil
}
func (s jmapBlobs) key(acct, id jmap.Id) (string, error) {
	root, err := jmapRoot(acct)
	if err != nil {
		return "", err
	}
	if !jmapBlobIDRE.MatchString(string(id)) {
		return "", jblob.ErrNotFound
	}
	return root + "/.jmap/blobs/" + string(id), nil
}
func (s jmapBlobs) Put(ctx context.Context, acct, id jmap.Id, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.key(acct, id); err != nil {
		return err
	}
	if id != jblob.IdFor(data) {
		return fmt.Errorf("JMAP blob content hash mismatch")
	}
	w, err := s.Create(ctx, acct)
	if err != nil {
		return err
	}
	defer w.Abort()
	if _, err = w.Write(data); err != nil {
		return err
	}
	_, err = w.Commit()
	return err
}
func (s jmapBlobs) Open(ctx context.Context, acct, id jmap.Id) (io.ReadCloser, int64, error) {
	key, err := s.key(acct, id)
	if err != nil {
		return nil, 0, err
	}
	r, size, err := openObject(ctx, s.store, key)
	if errors.Is(err, fs.ErrNotExist) {
		err = jblob.ErrNotFound
	}
	return r, size, err
}
func (s jmapBlobs) Delete(ctx context.Context, acct, id jmap.Id) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := s.key(acct, id)
	if err != nil {
		return err
	}
	err = s.store.Delete(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
func (s jmapBlobs) Create(ctx context.Context, acct jmap.Id) (jblob.BlobWriter, error) {
	root, err := jmapRoot(acct)
	if err != nil {
		return nil, err
	}
	writer, err := newObjectUpload(ctx, s.store, root+"/.jmap/uploads/")
	if err != nil {
		return nil, err
	}
	return &jmapBlobWriter{ctx: ctx, store: s, account: acct, writer: writer, digest: sha256.New()}, nil
}

type jmapBlobWriter struct {
	ctx      context.Context
	store    jmapBlobs
	account  jmap.Id
	writer   objectUpload
	digest   hash.Hash
	probe    []byte
	gzip     *gzip.Writer
	writeErr error
	size     int64
	finished bool
}

func (w *jmapBlobWriter) Write(data []byte) (int, error) {
	if w.finished {
		return 0, fs.ErrClosed
	}
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.size+int64(len(data)) > jmapMaxUpload {
		return 0, fmt.Errorf("JMAP upload exceeds 6 GiB")
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	var n int
	var err error
	if w.gzip == nil && w.size+int64(len(data)) <= gzipThreshold {
		w.probe = append(w.probe, data...)
		n = len(data)
	} else {
		if w.gzip == nil {
			w.gzip, err = gzip.NewWriterLevel(w.writer, gzip.BestSpeed)
			if err == nil {
				_, err = w.gzip.Write(w.probe)
			}
			w.probe = nil
			if err != nil {
				w.writeErr = err
				return 0, err
			}
		}
		n, err = w.gzip.Write(data)
	}
	w.writeErr = err
	w.digest.Write(data[:n])
	w.size += int64(n)
	return n, err
}
func (w *jmapBlobWriter) ID() jmap.Id {
	return jmap.Id("G" + base64.RawURLEncoding.EncodeToString(w.digest.Sum(nil)))
}
func (w *jmapBlobWriter) Commit() (jmap.Id, error) {
	if w.finished {
		return "", fs.ErrClosed
	}
	id := w.ID()
	key, err := w.store.key(w.account, id)
	if err != nil {
		return "", err
	}
	if w.writeErr != nil {
		return "", w.writeErr
	}
	var metadata map[string]string
	if w.gzip != nil {
		err = w.gzip.Close()
		metadata = map[string]string{"fma-encoding": "gzip", "fma-size": strconv.FormatInt(w.size, 10)}
	} else {
		_, err = w.writer.Write(w.probe)
		w.probe = nil
	}
	if err != nil {
		w.writeErr = err
		return "", err
	}
	if err = w.writer.Commit(key, metadata); err != nil {
		return "", err
	}
	w.finished = true
	return id, nil
}
func (w *jmapBlobWriter) Abort() error { w.finished = true; w.probe = nil; return w.writer.Abort() }

type jmapAccount struct {
	root  string
	id    jmap.Id
	db    *jdb.DB
	proc  *jruntime.Processor
	blobs jmapBlobs
	queue *jsubmit.Queue
}

func newJMAPAccount(root string, store objectStore) (*jmapAccount, error) {
	be := &jmapBackend{key: root + "/.jmap/state.json", store: store}
	a := &jmapAccount{root: root, id: jmapAccountID(root), blobs: jmapBlobs{store: store}, proc: jruntime.NewProcessor()}
	a.db = jdb.New(be, jlease.NewStoreLease(be, jlease.StoreLeaseConfig{}))
	core := jmapCore()
	for _, err := range []error{
		jmail.RegisterMailbox(a.proc, jmail.MailboxConfig{DB: a.db, Core: core}),
		jmail.RegisterThread(a.proc, jmail.ThreadConfig{DB: a.db, Core: core}),
		jmail.RegisterEmail(a.proc, jmail.EmailConfig{DB: a.db, Store: a.blobs, Core: core, AccountCapability: jmapMailCapability(), Searcher: jsearch.New(a.blobs, jsearch.DefaultConfig()), MessageIDDomain: config.Domain, InternalProperties: map[string]jdescriptor.Property{"fmaUIDs": {Kind: jdescriptor.KindObject, Default: json.RawMessage(`{}`)}}}),
		jmail.RegisterIdentity(a.proc, jmail.IdentityConfig{DB: a.db, Core: core, Policy: a}),
	} {
		if err != nil {
			return nil, err
		}
	}
	for name, prop := range map[string]jdescriptor.Property{
		"fmaKey":      {Kind: jdescriptor.KindString, Internal: true},
		"fmaValidity": {Kind: jdescriptor.KindUnsignedInt, Internal: true},
		"fmaNext":     {Kind: jdescriptor.KindUnsignedInt, Internal: true},
	} {
		a.db.Type("Mailbox").Properties[name] = prop
	}
	var err error
	limits := jmapSubmitLimits()
	a.queue, err = jsubmit.Register(a.proc, jsubmit.Config{DB: a.db, Store: a.blobs, Core: core, Policy: a, Limits: &limits})
	return a, err
}
func jmapCore() jmap.CoreCapabilities {
	c := jruntime.DefaultCoreCapabilities()
	c.MaxSizeUpload = jmapMaxUpload
	return c
}
func (a *jmapAccount) identity() *jauth.Identity {
	return &jauth.Identity{Username: a.root, Primary: a.id, Accounts: map[jmap.Id]jauth.Access{a.id: {Name: a.root + "@" + config.Domain, Personal: true}}}
}
func (a *jmapAccount) CanSend(ctx context.Context, id jmap.Id) (bool, string) {
	if err := ctx.Err(); err != nil {
		return false, err.Error()
	}
	if id != a.id {
		return false, "account not accessible"
	}
	root, err := resolveIdentity(a.root)
	return err == nil && root.RootID == a.root, "account is unavailable"
}
func (a *jmapAccount) CanSendAs(ctx context.Context, id jmap.Id, address string) bool {
	if ctx.Err() != nil || id != a.id {
		return false
	}
	user := localUser(address)
	if user == "" {
		return false
	}
	root, err := resolveIdentity(user)
	return err == nil && root.RootID == a.root
}
func jraw(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
func jvalue[T any](obj jdb.Object, key string) (value T) {
	_ = json.Unmarshal(obj[key], &value)
	return
}
func (a *jmapAccount) call(ctx context.Context, method string, request any) (jdb.Object, error) {
	var args jdb.Object
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &args); err != nil {
		return nil, err
	}
	if args == nil {
		args = jdb.Object{}
	}
	args["accountId"] = jraw(a.id)
	resp := a.proc.Process(ctx, &jmap.Request{Using: []string{jmap.CoreCapability, jmail.CapabilityURI, jsubmit.CapabilityURI}, MethodCalls: []jmap.Invocation{{Name: method, Args: jraw(args), CallID: "fma"}}}, a.identity(), "")
	if len(resp.MethodResponses) == 0 {
		return nil, fmt.Errorf("JMAP %s returned no response", method)
	}
	var obj jdb.Object
	if err := json.Unmarshal(resp.MethodResponses[0].Args, &obj); err != nil {
		return nil, err
	}
	if resp.MethodResponses[0].Name == "error" {
		return nil, fmt.Errorf("JMAP %s: %s", method, resp.MethodResponses[0].Args)
	}
	for _, key := range []string{"notCreated", "notUpdated", "notDestroyed"} {
		if len(jvalue[map[string]any](obj, key)) != 0 {
			return nil, fmt.Errorf("JMAP %s %s: %s", method, key, obj[key])
		}
	}
	return obj, nil
}
func (a *jmapAccount) all(ctx context.Context, typ string) ([]jdb.Object, error) {
	ids, err := a.db.AllIds(ctx, a.id, typ, 0)
	if err != nil {
		return nil, err
	}
	return a.db.GetMany(ctx, a.id, typ, ids)
}
func (a *jmapAccount) importMail(ctx context.Context, mailbox jmap.Id, body []byte, flags []string, date time.Time) (jmap.Id, error) {
	bw, err := a.blobs.Create(ctx, a.id)
	if err != nil {
		return "", err
	}
	defer bw.Abort()
	if _, err = bw.Write(body); err != nil {
		return "", err
	}
	blob, err := a.db.FinalizeBlobUpload(ctx, a.id, bw, a.root, time.Now())
	if err != nil {
		return "", err
	}
	if date.IsZero() {
		date = time.Now()
	}
	result, err := a.call(ctx, "Email/import", jmapImportRequest{Emails: map[string]jmapImportEmail{"fma": {BlobID: blob, MailboxIDs: map[jmap.Id]bool{mailbox: true}, Keywords: jmapKeywords(flags), ReceivedAt: date.UTC().Truncate(time.Second)}}})
	if err != nil {
		return "", err
	}
	return jvalue[jmap.Id](jvalue[map[string]jdb.Object](result, "created")["fma"], "id"), nil
}

var jmapFlagNames = map[string]string{imap.SeenFlag: "$seen", imap.AnsweredFlag: "$answered", imap.FlaggedFlag: "$flagged", imap.DraftFlag: "$draft", imap.DeletedFlag: "fma-deleted"}

func jmapKeywords(flags []string) map[string]bool {
	result := map[string]bool{}
	for _, f := range flags {
		if f == imap.RecentFlag {
			continue
		}
		if keyword, ok := jmapFlagNames[f]; ok {
			f = keyword
		}
		result[f] = true
	}
	return result
}
func jmapFlags(keywords map[string]bool) []string {
	var result []string
	for k, present := range keywords {
		if !present {
			continue
		}
		for flag, keyword := range jmapFlagNames {
			if k == keyword {
				k = flag
				break
			}
		}
		result = append(result, k)
	}
	sort.Strings(result)
	return result
}

// The legacy wire protocols project the same JMAP records into IMAP mailboxes.
// UIDs are private bookkeeping; changing them never advances JMAP public state.
func (a *jmapAccount) catalog(ctx context.Context) (map[string]folderMeta, map[string]jmap.Id, error) {
	boxes, err := a.all(ctx, "Mailbox")
	if err != nil {
		return nil, nil, err
	}
	byID := map[jmap.Id]jdb.Object{}
	for _, box := range boxes {
		byID[jvalue[jmap.Id](box, "id")] = box
	}
	var nameOf func(jmap.Id, int) string
	nameOf = func(id jmap.Id, depth int) string {
		b := byID[id]
		if b == nil || depth > 100 {
			return ""
		}
		name := jvalue[string](b, "name")
		if jvalue[string](b, "role") == "inbox" {
			return "INBOX"
		}
		if parent := jvalue[jmap.Id](b, "parentId"); parent != "" {
			name = nameOf(parent, depth+1) + "/" + name
		}
		return name
	}
	catalog, ids := map[string]folderMeta{}, map[string]jmap.Id{}
	for id, box := range byID {
		name := nameOf(id, 0)
		key := jvalue[string](box, "fmaKey")
		validity := jvalue[uint32](box, "fmaValidity")
		if key == "" {
			key = a.root + "/.folders/" + string(id)
			if jvalue[string](box, "role") == "inbox" {
				key = a.root
			}
		}
		if validity == 0 {
			sum := sha256.Sum256([]byte(id))
			validity = binary.BigEndian.Uint32(sum[:4]) | 1
			if key == a.root {
				validity = 1
			}
		}
		catalog[name] = folderMeta{Subscribed: jvalue[bool](box, "isSubscribed"), Validity: validity, Key: key}
		ids[key] = id
	}
	return catalog, ids, nil
}
func (a *jmapAccount) mailbox(ctx context.Context, key string) (jmap.Id, error) {
	_, ids, err := a.catalog(ctx)
	if err != nil {
		return "", err
	}
	if id := ids[key]; id != "" {
		return id, nil
	}
	return "", backend.ErrNoSuchMailbox
}
func (a *jmapAccount) private(ctx context.Context, typ string, id jmap.Id, metadata any) error {
	var values jdb.Object
	if err := json.Unmarshal(jraw(metadata), &values); err != nil {
		return err
	}
	_, err := a.db.Update(ctx, a.id, func(u *jdb.Update) error {
		obj, err := u.Get(typ, id)
		if err != nil {
			return err
		}
		obj = maps.Clone(obj)
		for k, v := range values {
			obj[k] = v
		}
		return u.PutInternal(typ, id, obj)
	})
	return err
}
func (a *jmapAccount) uid(ctx context.Context, boxID, emailID jmap.Id) (uint32, error) {
	var uid uint32
	_, err := a.db.Update(ctx, a.id, func(u *jdb.Update) error {
		email, err := u.Get("Email", emailID)
		if err != nil {
			return err
		}
		if !jvalue[map[jmap.Id]bool](email, "mailboxIds")[boxID] {
			return fs.ErrNotExist
		}
		uids := jvalue[map[jmap.Id]uint32](email, "fmaUIDs")
		uid = uids[boxID]
		if uid != 0 {
			return nil
		}
		if uids == nil {
			uids = map[jmap.Id]uint32{}
		}
		box, err := u.Get("Mailbox", boxID)
		if err != nil {
			return err
		}
		uid = jvalue[uint32](box, "fmaNext")
		if uid == 0 {
			uid = 1
		}
		if uid == ^uint32(0) {
			return fmt.Errorf("UID exhausted")
		}
		box = maps.Clone(box)
		box["fmaNext"] = jraw(uid + 1)
		if err = u.PutInternal("Mailbox", boxID, box); err != nil {
			return err
		}
		email = maps.Clone(email)
		uids[boxID] = uid
		email["fmaUIDs"] = jraw(uids)
		return u.PutInternal("Email", emailID, email)
	})
	return uid, err
}
func (a *jmapAccount) messages(ctx context.Context, key string) ([]*memory.Message, map[uint32]jmap.Id, error) {
	box, err := a.mailbox(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	emails, err := a.all(ctx, "Email")
	if err != nil {
		return nil, nil, err
	}
	result := []*memory.Message{}
	ids := map[uint32]jmap.Id{}
	for _, email := range emails {
		if !jvalue[map[jmap.Id]bool](email, "mailboxIds")[box] {
			continue
		}
		id := jvalue[jmap.Id](email, "id")
		uid := jvalue[map[jmap.Id]uint32](email, "fmaUIDs")[box]
		if uid == 0 {
			uid, err = a.uid(ctx, box, id)
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, jdb.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
		}
		r, size, err := a.blobs.Open(ctx, a.id, jvalue[jmap.Id](email, "blobId"))
		if err != nil {
			return nil, nil, err
		}
		body, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			return nil, nil, err
		}
		result = append(result, &memory.Message{Uid: uid, Date: jvalue[time.Time](email, "receivedAt"), Size: uint32(size), Flags: jmapFlags(jvalue[map[string]bool](email, "keywords")), Body: body})
		ids[uid] = id
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Uid < result[j].Uid })
	return result, ids, nil
}
func jmapForKey(key string) (*jmapAccount, error) {
	return openJMAPAccount(context.Background(), strings.SplitN(key, "/", 2)[0])
}
func jmapUpdateCatalog(user string, change func(map[string]folderMeta) error) error {
	ctx := context.Background()
	a, err := jmapForKey(user)
	if err != nil {
		return err
	}
	state, err := a.db.TypeState(ctx, a.id, "Mailbox")
	if err != nil {
		return err
	}
	before, ids, err := a.catalog(ctx)
	if err != nil {
		return err
	}
	after := maps.Clone(before)
	if err = change(after); err != nil {
		return err
	}
	create, update := map[string]jmapMailboxCreate{}, map[jmap.Id]jmapPatch{}
	destroy := []jmap.Id{}
	metas := map[string]folderMeta{}
	kept := map[string]bool{}
	for name, meta := range after {
		kept[meta.Key] = true
		if id := ids[meta.Key]; id != "" {
			if old, ok := before[name]; !ok || old != meta {
				update[id] = jmapPatch{"name": name, "isSubscribed": meta.Subscribed}
			}
		} else {
			token := fmt.Sprintf("f%d", len(create))
			create[token] = jmapMailboxCreate{Name: name, IsSubscribed: meta.Subscribed}
			metas[token] = meta
		}
	}
	for key, id := range ids {
		if !kept[key] {
			destroy = append(destroy, id)
		}
	}
	if len(create)+len(update)+len(destroy) == 0 {
		return nil
	}
	result, err := a.call(ctx, "Mailbox/set", jmapSetRequest[jmapMailboxCreate]{IfInState: state, Create: create, Update: update, Destroy: destroy, OnDestroyRemoveEmails: true})
	if err != nil {
		return err
	}
	for token, obj := range jvalue[map[string]jdb.Object](result, "created") {
		meta := metas[token]
		if err = a.private(ctx, "Mailbox", jvalue[jmap.Id](obj, "id"), jmapMailboxMetadata{Key: meta.Key, Validity: meta.Validity, Next: 1}); err != nil {
			return err
		}
	}
	return nil
}

// Bootstrap an existing mailbox in a private metadata transaction. Publishing
// uses Create, so concurrent nodes agree on one complete image; old objects stay
// untouched. Only immutable MIME blobs can precede that atomic publication.
type jmapBootstrapStore struct {
	objectStore
	key      string
	data     []byte
	revision int
}

func (s *jmapBootstrapStore) GetVersion(key string) ([]byte, string, error) {
	if key != s.key {
		return s.objectStore.(versionedStore).GetVersion(key)
	}
	if s.data == nil {
		return nil, "", fs.ErrNotExist
	}
	return bytes.Clone(s.data), strconv.Itoa(s.revision), nil
}
func (s *jmapBootstrapStore) Get(key string) ([]byte, error) {
	if key != s.key {
		return s.objectStore.Get(key)
	}
	data, _, err := s.GetVersion(key)
	return data, err
}
func (s *jmapBootstrapStore) Create(key string, data []byte) error {
	if key != s.key {
		return s.objectStore.Create(key, data)
	}
	if s.data != nil {
		return fs.ErrExist
	}
	s.data = bytes.Clone(data)
	s.revision++
	return nil
}
func (s *jmapBootstrapStore) Swap(key string, data []byte, etag string) error {
	if key != s.key {
		return s.objectStore.(versionedStore).Swap(key, data, etag)
	}
	if etag != strconv.Itoa(s.revision) {
		return fs.ErrExist
	}
	s.data = bytes.Clone(data)
	s.revision++
	return nil
}
func openJMAPAccount(ctx context.Context, root string) (*jmapAccount, error) {
	if !usernameRE.MatchString(root) {
		return nil, jauth.ErrUnauthenticated
	}
	key := root + "/.jmap/state.json"
	if _, err := objects.Get(key); err == nil {
		return newJMAPAccount(root, objects)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	staged := &jmapBootstrapStore{objectStore: objects, key: key}
	a, err := newJMAPAccount(root, staged)
	if err != nil {
		return nil, err
	}
	catalog, err := legacyFolderCatalog(root)
	if err != nil {
		return nil, err
	}
	for _, name := range standardFolders {
		if _, ok := catalog[name]; !ok {
			meta, err := newFolderMeta(root, name)
			if err != nil {
				return nil, err
			}
			catalog[name] = meta
		}
	}
	names := make([]string, 0, len(catalog))
	for name := range catalog {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		meta := catalog[name]
		box := jmapMailboxCreate{Name: name, IsSubscribed: meta.Subscribed}
		for _, standard := range standardFolders {
			if name == standard {
				box.Role = strings.ToLower(standard)
			}
		}
		result, err := a.call(ctx, "Mailbox/set", jmapSetRequest[jmapMailboxCreate]{Create: map[string]jmapMailboxCreate{"fma": box}})
		if err != nil {
			return nil, err
		}
		id := jvalue[jmap.Id](jvalue[map[string]jdb.Object](result, "created")["fma"], "id")
		next, err := legacyNextUID(meta.Key)
		if err != nil {
			return nil, err
		}
		emails, err := legacyMessages(meta.Key)
		if err != nil {
			return nil, err
		}
		for _, email := range emails {
			eid, err := a.importMail(ctx, id, email.Body, email.Flags, email.Date)
			if err != nil {
				return nil, err
			}
			if err = a.private(ctx, "Email", eid, jmapEmailMetadata{UIDs: map[jmap.Id]uint32{id: email.Uid}}); err != nil {
				return nil, err
			}
			if email.Uid >= next {
				next = email.Uid + 1
			}
		}
		if err = a.private(ctx, "Mailbox", id, jmapMailboxMetadata{Key: meta.Key, Validity: meta.Validity, Next: next}); err != nil {
			return nil, err
		}
	}
	if _, err = a.call(ctx, "Identity/set", jmapSetRequest[jmapIdentityCreate]{Create: map[string]jmapIdentityCreate{"fma": {Name: root, Email: root + "@" + config.Domain}}}); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = objects.Create(key, staged.data); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return newJMAPAccount(root, objects)
}

var useJMAP bool

func folderCatalog(user string) (map[string]folderMeta, error) {
	if !useJMAP {
		return legacyFolderCatalog(user)
	}
	a, err := jmapForKey(user)
	if err != nil {
		return nil, err
	}
	catalog, _, err := a.catalog(context.Background())
	return catalog, err
}
func messages(key string) ([]*memory.Message, error) {
	if !useJMAP {
		return legacyMessages(key)
	}
	a, err := jmapForKey(key)
	if err != nil {
		return nil, err
	}
	messages, _, err := a.messages(context.Background(), key)
	return messages, err
}
func nextUID(key string) (uint32, error) {
	if !useJMAP {
		return legacyNextUID(key)
	}
	a, err := jmapForKey(key)
	if err != nil {
		return 0, err
	}
	ctx := context.Background()
	id, err := a.mailbox(ctx, key)
	if err != nil {
		return 0, err
	}
	box, err := a.db.Get(ctx, a.id, "Mailbox", id)
	if err != nil {
		return 0, err
	}
	return max(1, jvalue[uint32](box, "fmaNext")), nil
}

type jmapAuth struct{ identity *jauth.Identity }

func (a jmapAuth) Authenticate(r *http.Request) (*jauth.Identity, error) { return a.identity, nil }

var jmapHTTPSlots = make(chan struct{}, 4)

func serveJMAP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "fma mail server: SMTP, POP3, IMAP and JMAP")
		return
	}
	switch {
	case r.URL.Path == "/.well-known/jmap", r.URL.Path == "/api", r.URL.Path == "/eventsource", strings.HasPrefix(r.URL.Path, "/upload/"), strings.HasPrefix(r.URL.Path, "/download/"):
	default:
		http.NotFound(w, r)
		return
	}
	user, password, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="fma", charset="UTF-8"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	identity, err := authenticateAccount(user, password)
	if err != nil {
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}
	select {
	case jmapHTTPSlots <- struct{}{}:
		defer func() { <-jmapHTTPSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	a, err := openJMAPAccount(r.Context(), identity.RootID)
	if err != nil {
		logger.Error("JMAP account", "error", err)
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	partIDs := map[jmap.Id]bool{}
	if strings.HasPrefix(r.URL.Path, "/download/") {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/download/"), "/")
		if len(parts) == 3 && parts[0] == string(a.id) {
			partIDs[jmap.Id(parts[1])] = true
		}
	}
	if r.URL.Path == "/api" && r.Method == http.MethodPost {
		data, err := io.ReadAll(io.LimitReader(r.Body, jmapCore().MaxSizeRequest+1))
		r.Body.Close()
		if err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		var request any
		if int64(len(data)) <= jmapCore().MaxSizeRequest && json.Unmarshal(data, &request) == nil {
			jmapReferencedBlobs(request, partIDs)
		}
	}
	for id := range partIDs {
		if err = a.materializePart(r.Context(), id, identity.LoginID); err != nil && !errors.Is(err, jblob.ErrNotFound) {
			logger.Error("JMAP attachment", "error", err)
			http.Error(w, "attachment storage unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	auth := a.identity()
	auth.Username = identity.LoginID
	base := config.JMAPURL
	if base == "" {
		base = "https://mail." + config.Domain
	}
	srv, err := jruntime.NewServer(jmapAuth{auth}, a.proc, base, jmapCore())
	if err != nil {
		logger.Error("JMAP server", "error", err)
		http.Error(w, "JMAP unavailable", http.StatusServiceUnavailable)
		return
	}
	defer srv.Close()
	srv.EnableBlobs(a.db, a.blobs)
	if err = srv.Capability(jmail.CapabilityURI).Advertise(struct{}{}, jmapMailCapability()).Err(); err == nil {
		err = srv.Capability(jsubmit.CapabilityURI).Advertise(map[string]any{}, jsubmit.AccountCapabilityFor(jmapSubmitLimits())).Err()
	}
	if err != nil {
		logger.Error("JMAP capabilities", "error", err)
		http.Error(w, "JMAP unavailable", http.StatusServiceUnavailable)
		return
	}
	srv.ServeHTTP(w, r)
}
func jmapRemoveMessage(key string, uid uint32) error {
	a, err := jmapForKey(key)
	if err != nil {
		return err
	}
	ctx := context.Background()
	_, ids, err := a.messages(ctx, key)
	if err != nil {
		return err
	}
	id := ids[uid]
	if id == "" {
		return nil
	}
	box, err := a.mailbox(ctx, key)
	if err != nil {
		return err
	}
	email, err := a.db.Get(ctx, a.id, "Email", id)
	if errors.Is(err, jdb.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	boxes := jvalue[map[jmap.Id]bool](email, "mailboxIds")
	args := map[string]any{"destroy": []jmap.Id{id}}
	if len(boxes) > 1 {
		args = map[string]any{"update": map[jmap.Id]any{id: map[string]any{"mailboxIds/" + string(box): nil}}}
	}
	_, err = a.call(ctx, "Email/set", args)
	return err
}
func jmapUpdateFlags(key string, uid uint32, op imap.FlagsOp, flags []string) ([]string, error) {
	a, err := jmapForKey(key)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	_, ids, err := a.messages(ctx, key)
	if err != nil {
		return nil, err
	}
	id := ids[uid]
	if id == "" {
		return nil, fs.ErrNotExist
	}
	email, err := a.db.Get(ctx, a.id, "Email", id)
	if err != nil {
		return nil, err
	}
	updated := backendutil.UpdateFlags(jmapFlags(jvalue[map[string]bool](email, "keywords")), op, flags)
	patch := map[string]any{"keywords": jmapKeywords(updated)}
	if op != imap.SetFlags {
		patch = map[string]any{}
		for keyword := range jmapKeywords(flags) {
			var value any = true
			if op == imap.RemoveFlags {
				value = nil
			}
			patch["keywords/"+strings.ReplaceAll(strings.ReplaceAll(keyword, "~", "~0"), "/", "~1")] = value
		}
	}
	_, err = a.call(ctx, "Email/set", map[string]any{"update": map[jmap.Id]any{id: patch}})
	return updated, err
}

type jmapSubmitter struct{ root string }

func (sender jmapSubmitter) Submit(ctx context.Context, env jsubmit.Envelope, r io.Reader) ([]jsubmit.Result, error) {
	ref, err := storeMailStream(ctx, sender.root, r)
	if err != nil {
		return nil, err
	}
	var results []jsubmit.Result
	for _, rcpt := range env.Recipients {
		if err = ctx.Err(); err != nil {
			return results, err
		}
		var sendErr error
		address := rcpt.Email
		if user := localUser(address); user != "" {
			local, remote, _, err := resolveDelivery(user)
			if err != nil {
				sendErr = &textproto.Error{Code: 550, Msg: "recipient unavailable"}
			} else if local != "" {
				sendErr = deliverMailStream(ctx, []string{local}, ref, false)
			} else {
				address = remote
			}
			if err == nil && local != "" {
				address = ""
			}
		}
		if sendErr == nil && address != "" {
			if !outboundEnabled() {
				sendErr = &textproto.Error{Code: 550, Msg: "external delivery disabled"}
			} else {
				sendErr = sendRemote(ctx, &outboundJob{From: env.MailFrom, Blob: ref}, address)
			}
		}
		result := jsubmit.Result{Recipient: rcpt.Email, Outcome: jmail.Accepted, Reply: "250 2.0.0 delivered"}
		if sendErr != nil {
			result.Outcome = jmail.TempFailed
			result.Reply = "451 4.0.0 " + sendErr.Error()
			var response *textproto.Error
			if errors.As(sendErr, &response) && response.Code >= 500 {
				result.Outcome = jmail.Rejected
				result.Reply = response.Error()
			}
		}
		results = append(results, result)
	}
	return results, nil
}
func scanJMAPTasks(ctx context.Context, lease *bucketLease, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	keys, err := objects.List(".jmap-queue/")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, key := range keys {
		if ctx.Err() != nil || !lease.valid(time.Now()) || count >= limit {
			break
		}
		root, err := jmapRoot(jmap.Id(path.Base(key)))
		if err != nil {
			continue
		}
		a, err := newJMAPAccount(root, objects)
		if err != nil {
			return count, err
		}
		cfg := jsubmit.DefaultWorkerConfig()
		cfg.ClaimWindow = 20 * time.Second
		cfg.TransmitTimeout = 6 * time.Second
		cfg.BatchSize = 1
		cfg.QueueScanInterval = taskScanEvery
		worker, err := jsubmit.NewWorker(a.queue, jmapSubmitter{root: root}, cfg)
		if err != nil {
			return count, err
		}
		// A short sweep claims one item at a time, checking the scanner lease
		// before each further claim. The library fences and clears each claim.
		for count < limit && lease.valid(time.Now()) && ctx.Err() == nil {
			sent, _, err := worker.ProcessDue(ctx, 1)
			count += sent
			if err != nil {
				return count, err
			}
			if sent == 0 {
				break
			}
		}
	}
	return count, nil
}

func jmapMailCapability() jmail.AccountCapability {
	c := jmail.DefaultAccountCapability()
	c.MaxSizeAttachmentsPerEmail = maxAttachmentSize
	return c
}
func jmapSubmitLimits() jsubmit.Limits {
	c := jsubmit.DefaultLimits()
	c.MaxMessageBytes = maxMailSize
	return c
}

// Wire structures owned by fma. The library owns JMAP methods, errors,
// envelopes, states, MIME body parts and all public Email records.
type jmapSetRequest[T any] struct {
	AccountID             jmap.Id               `json:"accountId,omitempty"`
	IfInState             string                `json:"ifInState,omitempty"`
	Create                map[string]T          `json:"create,omitempty"`
	Update                map[jmap.Id]jmapPatch `json:"update,omitempty"`
	Destroy               []jmap.Id             `json:"destroy,omitempty"`
	OnDestroyRemoveEmails bool                  `json:"onDestroyRemoveEmails,omitempty"`
}
type jmapPatch map[string]any

type jmapMailboxCreate struct {
	Name         string   `json:"name"`
	ParentID     *jmap.Id `json:"parentId,omitempty"`
	Role         string   `json:"role,omitempty"`
	IsSubscribed bool     `json:"isSubscribed"`
}
type jmapIdentityCreate struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}
type jmapImportEmail struct {
	BlobID     jmap.Id          `json:"blobId"`
	MailboxIDs map[jmap.Id]bool `json:"mailboxIds"`
	Keywords   map[string]bool  `json:"keywords"`
	ReceivedAt time.Time        `json:"receivedAt"`
}
type jmapImportRequest struct {
	AccountID jmap.Id                    `json:"accountId,omitempty"`
	Emails    map[string]jmapImportEmail `json:"emails"`
}
type jmapMailboxMetadata struct {
	Key      string `json:"fmaKey"`
	Validity uint32 `json:"fmaValidity"`
	Next     uint32 `json:"fmaNext"`
}
type jmapEmailMetadata struct {
	UIDs map[jmap.Id]uint32 `json:"fmaUIDs"`
}

// naust-jmap returns hashes for MIME parts, but its blob provider stores whole
// messages. Resolve a missing part only through an Email in this account; an
// unreferenced blob or another account's hash never grants access.
func (a *jmapAccount) materializePart(ctx context.Context, id jmap.Id, uploader string) error {
	if !jmapBlobIDRE.MatchString(string(id)) {
		return jblob.ErrNotFound
	}
	ident := a.identity()
	ident.Username = uploader
	if r, _, err := jruntime.OpenBlob(ctx, a.db, a.blobs, a.id, id, ident); err == nil {
		r.Close()
		return nil
	} else if !errors.Is(err, jblob.ErrNotFound) {
		return err
	}
	emails, err := a.all(ctx, "Email")
	if err != nil {
		return err
	}
	for _, email := range emails {
		r, _, err := a.blobs.Open(ctx, a.id, jvalue[jmap.Id](email, "blobId"))
		if err != nil {
			return err
		}
		msg, err := mail.ReadMessage(r)
		if err != nil {
			r.Close()
			continue
		}
		part, found, err := jmapFindPart(ctx, textproto.MIMEHeader(msg.Header), msg.Body, id, 0)
		r.Close()
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		bw, err := a.blobs.Create(ctx, a.id)
		if err != nil {
			return err
		}
		defer bw.Abort()
		if _, err = bw.Write(part); err != nil {
			return err
		}
		_, err = a.db.FinalizeBlobUpload(ctx, a.id, bw, uploader, time.Now())
		return err
	}
	return jblob.ErrNotFound
}
func jmapFindPart(ctx context.Context, h textproto.MIMEHeader, r io.Reader, wanted jmap.Id, depth int) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if depth > 30 {
		return nil, false, fmt.Errorf("MIME nesting exceeds 30")
	}
	switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	}
	typ, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		typ = "text/plain"
	}
	if strings.HasPrefix(typ, "multipart/") {
		parts := multipart.NewReader(r, params["boundary"])
		for {
			p, err := parts.NextRawPart()
			if errors.Is(err, io.EOF) {
				return nil, false, nil
			}
			if err != nil {
				return nil, false, err
			}
			data, found, err := jmapFindPart(ctx, p.Header, p, wanted, depth+1)
			p.Close()
			if found || err != nil {
				return data, found, err
			}
		}
	}
	data, err := readMail(r)
	if err != nil {
		return nil, false, err
	}
	if jblob.IdFor(data) == wanted {
		return data, true, nil
	}
	if typ == "message/rfc822" {
		msg, err := mail.ReadMessage(bytes.NewReader(data))
		if err == nil {
			return jmapFindPart(ctx, textproto.MIMEHeader(msg.Header), msg.Body, wanted, depth+1)
		}
	}
	return nil, false, nil
}
func jmapReferencedBlobs(value any, ids map[jmap.Id]bool) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			switch key {
			case "blobId":
				if id, ok := child.(string); ok {
					ids[jmap.Id(id)] = true
				}
			case "blobIds":
				if list, ok := child.([]any); ok {
					for _, entry := range list {
						if id, ok := entry.(string); ok {
							ids[jmap.Id(id)] = true
						}
					}
				}
			default:
				jmapReferencedBlobs(child, ids)
			}
		}
	case []any:
		for _, child := range value {
			jmapReferencedBlobs(child, ids)
		}
	}
}

// Streaming object IO is separate from small, conditional metadata operations.
// Multipart uploads keep at most one 8 MiB part in memory and use S3, never disk,
// for staging while the content-addressed blob ID is still being calculated.
const s3PartSize = 8 << 20
const gzipThreshold = 10 << 20
const (
	maxAttachmentSize = 4 << 30
	// Base64 with CRLF every 76 characters expands 4 GiB to about 5.48 GiB.
	maxMailSize         = 6 << 30
	jmapMaxUpload int64 = maxMailSize // Email/import uploads complete MIME too.
)

type streamingStore interface {
	OpenStream(context.Context, string) (io.ReadCloser, int64, error)
	NewUpload(context.Context, string) (objectUpload, error)
	CopyStream(context.Context, string, string) error
}
type objectUpload interface {
	io.Writer
	Commit(string, map[string]string) error
	Abort() error
}

func openObject(ctx context.Context, store objectStore, key string) (io.ReadCloser, int64, error) {
	if s, ok := store.(streamingStore); ok {
		return s.OpenStream(ctx, key)
	}
	data, err := store.Get(key)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}
func newObjectUpload(ctx context.Context, store objectStore, prefix string) (objectUpload, error) {
	if s, ok := store.(streamingStore); ok {
		return s.NewUpload(ctx, prefix)
	}
	return &bufferedObjectUpload{ctx: ctx, store: store}, nil // Small in-memory test stores only.
}

type bufferedObjectUpload struct {
	bytes.Buffer
	ctx   context.Context
	store objectStore
}

func (w *bufferedObjectUpload) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.Len()+len(p) > s3PartSize {
		return 0, fmt.Errorf("store does not implement streaming uploads")
	}
	return w.Buffer.Write(p)
}
func (w *bufferedObjectUpload) Commit(key string, metadata map[string]string) error {
	if len(metadata) != 0 {
		return fmt.Errorf("test store does not support compressed object metadata")
	}
	err := w.store.Create(key, w.Bytes())
	if errors.Is(err, fs.ErrExist) {
		err = nil
	}
	w.Reset()
	return err
}
func (w *bufferedObjectUpload) Abort() error { w.Reset(); return nil }
func (b *s3Bucket) OpenStream(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	result, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &b.bucket, Key: &key})
	if err != nil {
		return nil, 0, objectError(key, err)
	}
	if result.Metadata["fma-encoding"] == "gzip" {
		size, err := strconv.ParseInt(result.Metadata["fma-size"], 10, 64)
		if err != nil || size < 0 || size > jmapMaxUpload {
			result.Body.Close()
			return nil, 0, fmt.Errorf("invalid uncompressed blob size")
		}
		reader, err := gzip.NewReader(result.Body)
		if err != nil {
			result.Body.Close()
			return nil, 0, err
		}
		return &gzipObjectReader{Reader: reader, source: result.Body}, size, nil
	}
	return result.Body, aws.ToInt64(result.ContentLength), nil
}

type gzipObjectReader struct {
	*gzip.Reader
	source io.ReadCloser
}

func (r *gzipObjectReader) Close() error { return errors.Join(r.Reader.Close(), r.source.Close()) }
func (b *s3Bucket) CopyStream(ctx context.Context, src, dst string) error {
	return b.copyStream(ctx, src, dst, nil)
}
func (b *s3Bucket) copyStream(ctx context.Context, src, dst string, metadata map[string]string) error {
	head, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b.bucket, Key: &src})
	if err != nil {
		return objectError(src, err)
	}
	if metadata == nil {
		metadata = head.Metadata
	}
	source := url.PathEscape(b.bucket + "/" + src)
	size := aws.ToInt64(head.ContentLength)
	if size <= 5<<30 {
		_, err = b.client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &b.bucket, Key: &dst, CopySource: &source, CopySourceIfMatch: head.ETag, Metadata: metadata, MetadataDirective: s3types.MetadataDirectiveReplace})
		return objectError(dst, err)
	}
	// CopyObject cannot copy a complete MIME object above 5 GiB.
	upload, err := b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &b.bucket, Key: &dst, Metadata: metadata})
	if err != nil {
		return objectError(dst, err)
	}
	complete := false
	defer func() {
		if !complete {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if _, e := b.client.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{Bucket: &b.bucket, Key: &dst, UploadId: upload.UploadId}); e != nil {
				logger.Warn("abort multipart copy", "key", dst, "error", e)
			}
		}
	}()
	var parts []s3types.CompletedPart
	for offset := int64(0); offset < size; offset += 512 << 20 {
		number := int32(len(parts) + 1)
		rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, min(offset+(512<<20), size)-1)
		part, e := b.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: &b.bucket, Key: &dst, UploadId: upload.UploadId, PartNumber: &number, CopySource: &source, CopySourceIfMatch: head.ETag, CopySourceRange: &rangeHeader})
		if e != nil {
			return objectError(dst, e)
		}
		if part.CopyPartResult == nil || part.CopyPartResult.ETag == nil {
			return fmt.Errorf("S3 multipart copy returned no ETag")
		}
		parts = append(parts, s3types.CompletedPart{PartNumber: &number, ETag: part.CopyPartResult.ETag})
	}
	_, err = b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &b.bucket, Key: &dst, UploadId: upload.UploadId, MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts}})
	complete = err == nil
	return objectError(dst, err)
}
func (b *s3Bucket) NewUpload(ctx context.Context, prefix string) (objectUpload, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	return &s3StreamUpload{bucket: b, ctx: ctx, key: prefix + hex.EncodeToString(id[:])}, ctx.Err()
}

type s3StreamUpload struct {
	bucket   *s3Bucket
	ctx      context.Context
	key      string
	uploadID *string
	parts    []s3types.CompletedPart
	buffer   []byte
	finished bool
}

func (w *s3StreamUpload) Write(data []byte) (int, error) {
	if w.finished {
		return 0, fs.ErrClosed
	}
	written := 0
	for len(data) > 0 {
		if err := w.ctx.Err(); err != nil {
			return written, err
		}
		n := min(s3PartSize-len(w.buffer), len(data))
		w.buffer = append(w.buffer, data[:n]...)
		data = data[n:]
		written += n
		if len(w.buffer) == s3PartSize {
			if err := w.flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}
func (w *s3StreamUpload) flush() error {
	b := w.bucket
	if w.uploadID == nil {
		result, err := b.client.CreateMultipartUpload(w.ctx, &s3.CreateMultipartUploadInput{Bucket: &b.bucket, Key: &w.key})
		if err != nil {
			return objectError(w.key, err)
		}
		w.uploadID = result.UploadId
	}
	number := int32(len(w.parts) + 1)
	result, err := b.client.UploadPart(w.ctx, &s3.UploadPartInput{Bucket: &b.bucket, Key: &w.key, UploadId: w.uploadID, PartNumber: &number, Body: bytes.NewReader(w.buffer), ContentLength: aws.Int64(int64(len(w.buffer)))})
	if err != nil {
		return objectError(w.key, err)
	}
	w.parts = append(w.parts, s3types.CompletedPart{ETag: result.ETag, PartNumber: &number})
	w.buffer = w.buffer[:0]
	return nil
}
func (w *s3StreamUpload) Commit(key string, metadata map[string]string) error {
	if w.finished {
		return fs.ErrClosed
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	b := w.bucket
	if w.uploadID == nil {
		_, err := b.client.PutObject(w.ctx, &s3.PutObjectInput{Bucket: &b.bucket, Key: &key, Body: bytes.NewReader(w.buffer), IfNoneMatch: aws.String("*"), Metadata: metadata})
		if err = objectError(key, err); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	} else {
		if len(w.buffer) > 0 {
			if err := w.flush(); err != nil {
				return err
			}
		}
		_, err := b.client.CompleteMultipartUpload(w.ctx, &s3.CompleteMultipartUploadInput{Bucket: &b.bucket, Key: &w.key, UploadId: w.uploadID, MultipartUpload: &s3types.CompletedMultipartUpload{Parts: w.parts}})
		if err != nil {
			return objectError(w.key, err)
		}
		if err = b.copyStream(w.ctx, w.key, key, metadata); err != nil {
			return err
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), 30*time.Second)
		defer cancel()
		if _, err = b.client.DeleteObject(cleanup, &s3.DeleteObjectInput{Bucket: &b.bucket, Key: &w.key}); err != nil {
			logger.Warn("remove completed S3 upload staging object", "key", w.key, "error", err)
		}
	}
	w.finished = true
	w.buffer = nil
	return nil
}
func (w *s3StreamUpload) Abort() error {
	if w.finished {
		return nil
	}
	w.finished = true
	w.buffer = nil
	if w.uploadID == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), 30*time.Second)
	defer cancel()
	b := w.bucket
	_, err := b.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &b.bucket, Key: &w.key, UploadId: w.uploadID})
	// Complete may have succeeded before a failed Copy; remove that staging key too.
	_, delErr := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b.bucket, Key: &w.key})
	if err = objectError(w.key, err); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return objectError(w.key, delErr)
}

type guardedReader struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (r *guardedReader) Close() error { err := r.ReadCloser.Close(); r.once.Do(r.release); return err }

type guardedUpload struct {
	objectUpload
	once    sync.Once
	release func()
}

func (w *guardedUpload) Commit(key string, metadata map[string]string) error {
	err := w.objectUpload.Commit(key, metadata)
	if err == nil {
		w.once.Do(w.release)
	}
	return err
}
func (w *guardedUpload) Abort() error {
	err := w.objectUpload.Abort()
	w.once.Do(w.release)
	return err
}
func (g *guardedStore) OpenStream(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	g.mu.RLock()
	if g.closed {
		g.mu.RUnlock()
		return nil, 0, net.ErrClosed
	}
	r, size, err := openObject(ctx, g.base, key)
	if err != nil {
		g.mu.RUnlock()
		return nil, 0, err
	}
	return &guardedReader{ReadCloser: r, release: g.mu.RUnlock}, size, nil
}
func (g *guardedStore) NewUpload(ctx context.Context, prefix string) (objectUpload, error) {
	g.mu.RLock()
	if g.closed {
		g.mu.RUnlock()
		return nil, net.ErrClosed
	}
	w, err := newObjectUpload(ctx, g.base, prefix)
	if err != nil {
		g.mu.RUnlock()
		return nil, err
	}
	return &guardedUpload{objectUpload: w, release: g.mu.RUnlock}, nil
}
func (g *guardedStore) CopyStream(ctx context.Context, src, dst string) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed {
		return net.ErrClosed
	}
	return g.base.(streamingStore).CopyStream(ctx, src, dst)
}
func (s *jmapBootstrapStore) OpenStream(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	return openObject(ctx, s.objectStore, key)
}
func (s *jmapBootstrapStore) NewUpload(ctx context.Context, prefix string) (objectUpload, error) {
	return newObjectUpload(ctx, s.objectStore, prefix)
}
func (s *jmapBootstrapStore) CopyStream(ctx context.Context, src, dst string) error {
	return s.objectStore.(streamingStore).CopyStream(ctx, src, dst)
}

type jmapBlobRef struct {
	AccountID jmap.Id `json:"accountId"`
	BlobID    jmap.Id `json:"blobId"`
	Size      int64   `json:"size"`
}

func storeMailStream(ctx context.Context, root string, r io.Reader) (*jmapBlobRef, error) {
	store := jmapBlobs{store: objects}
	account := jmapAccountID(root)
	writer, err := store.Create(ctx, account)
	if err != nil {
		return nil, err
	}
	defer writer.Abort()
	size, err := io.Copy(writer, io.LimitReader(r, maxMailSize+1))
	if err != nil {
		return nil, err
	}
	if size > maxMailSize {
		return nil, &smtp.SMTPError{Code: 552, Message: "message too large"}
	}
	id, err := writer.Commit()
	if err != nil {
		return nil, err
	}
	return &jmapBlobRef{AccountID: account, BlobID: id, Size: size}, nil
}

type jmapExistingBlob struct {
	ctx     context.Context
	store   jmapBlobs
	source  *jmapBlobRef
	account jmap.Id
}

func (w jmapExistingBlob) Write([]byte) (int, error) { return 0, fmt.Errorf("immutable blob") }
func (w jmapExistingBlob) ID() jmap.Id               { return w.source.BlobID }
func (w jmapExistingBlob) Abort() error              { return nil }
func (w jmapExistingBlob) Commit() (jmap.Id, error) {
	id := w.ID()
	if w.source.AccountID == w.account {
		return id, nil
	}
	src, err := w.store.key(w.source.AccountID, id)
	if err != nil {
		return "", err
	}
	dst, err := w.store.key(w.account, id)
	if err != nil {
		return "", err
	}
	if stream, ok := w.store.store.(streamingStore); ok {
		err = stream.CopyStream(w.ctx, src, dst)
	} else {
		var data []byte
		data, err = w.store.store.Get(src)
		if err == nil {
			err = w.store.Put(w.ctx, w.account, id, data)
		}
	}
	return id, err
}
func (a *jmapAccount) importStoredMail(ctx context.Context, box jmap.Id, ref *jmapBlobRef, flags []string, date time.Time, unique bool) (jmap.Id, error) {
	if unique {
		emails, err := a.all(ctx, "Email")
		if err != nil {
			return "", err
		}
		for _, email := range emails {
			if jvalue[jmap.Id](email, "blobId") == ref.BlobID && jvalue[map[jmap.Id]bool](email, "mailboxIds")[box] {
				return jvalue[jmap.Id](email, "id"), nil
			}
		}
	}
	blob, err := a.db.FinalizeBlobUpload(ctx, a.id, jmapExistingBlob{ctx: ctx, store: a.blobs, source: ref, account: a.id}, a.root, time.Now())
	if err != nil {
		return "", err
	}
	if date.IsZero() {
		date = time.Now()
	}
	result, err := a.call(ctx, "Email/import", jmapImportRequest{Emails: map[string]jmapImportEmail{"fma": {BlobID: blob, MailboxIDs: map[jmap.Id]bool{box: true}, Keywords: jmapKeywords(flags), ReceivedAt: date.UTC().Truncate(time.Second)}}})
	if err != nil {
		return "", err
	}
	id := jvalue[jmap.Id](jvalue[map[string]jdb.Object](result, "created")["fma"], "id")
	_, err = a.uid(ctx, box, id)
	return id, err
}
func deliverMailStream(ctx context.Context, users []string, ref *jmapBlobRef, sent bool) error {
	for _, user := range users {
		if user == "" {
			continue
		}
		a, err := openJMAPAccount(ctx, user)
		if err != nil {
			return err
		}
		catalog, _, err := a.catalog(ctx)
		if err != nil {
			return err
		}
		key := user
		if sent {
			key = catalog["Sent"].Key
		}
		box, err := a.mailbox(ctx, key)
		if err != nil {
			return err
		}
		if _, err = a.importStoredMail(ctx, box, ref, nil, time.Now(), sent); err != nil {
			return err
		}
	}
	return nil
}
func (job *outboundJob) openBody(ctx context.Context) (io.ReadCloser, error) {
	if job.Blob == nil {
		return io.NopCloser(bytes.NewReader(job.Body)), nil
	}
	reader, _, err := (jmapBlobs{store: objects}).Open(ctx, job.Blob.AccountID, job.Blob.BlobID)
	if err != nil {
		return nil, err
	}
	return &prefixedReader{Reader: io.MultiReader(strings.NewReader(job.Prefix), reader), closer: reader}, nil
}

type prefixedReader struct {
	io.Reader
	closer io.Closer
}

func (r *prefixedReader) Close() error { return r.closer.Close() }
func proxyStreamPrefix(ctx context.Context, ref *jmapBlobRef) (string, error) {
	r, _, err := (jmapBlobs{store: objects}).Open(ctx, ref.AccountID, ref.BlobID)
	if err != nil {
		return "", err
	}
	defer r.Close()
	msg, err := mail.ReadMessage(io.LimitReader(r, 64<<10))
	if err != nil {
		return "", err
	}
	hops := 0
	for _, value := range msg.Header["X-Fma-Proxy-Hops"] {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 0 {
			return "", fmt.Errorf("invalid proxy hop count")
		}
		hops = max(hops, n)
	}
	if hops >= 16 {
		return "", fmt.Errorf("proxy forwarding loop limit reached")
	}
	return fmt.Sprintf("X-FMA-Proxy-Hops: %d\r\n", hops+1), nil
}
func (s *smtpSession) streamData(r io.Reader) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	owner := s.user
	if owner == "" && len(s.recipients) > 0 {
		owner = s.recipients[0]
	}
	if owner == "" && len(s.proxies) > 0 {
		owner = s.proxies[0].Owner
	}
	if owner == "" {
		return fmt.Errorf("message has no storage owner")
	}
	ref, err := storeMailStream(ctx, owner, r)
	if err != nil {
		return err
	}
	job := &outboundJob{ID: string(jmap.NewId()), User: s.user, From: s.from, Blob: ref, Created: time.Now(), Archived: true}
	if len(s.proxies) > 0 {
		job.Prefix, err = proxyStreamPrefix(ctx, ref)
		if err != nil {
			return err
		}
	}
	if err = deliverMailStream(ctx, s.recipients, ref, false); err != nil {
		return err
	}
	if err = deliverMailStream(ctx, []string{s.user}, ref, true); err != nil {
		return err
	}
	for _, address := range s.remote {
		recipient := outboundRecipient{Address: address, State: "pending"}
		for _, proxy := range s.proxies {
			if proxy.Address == address && !slices.Contains(recipient.ProxyOwners, proxy.Owner) {
				recipient.ProxyOwners = append(recipient.ProxyOwners, proxy.Owner)
			}
		}
		job.Recipients = append(job.Recipients, recipient)
	}
	if len(job.Recipients) > 0 {
		if err = writeJSON(path.Join(outbox, job.ID+".json"), job); err != nil {
			return err
		}
	}
	logger.Info("SMTP DATA stored as S3 stream", "user", s.user, "bytes", ref.Size, "local", len(s.recipients), "remote", len(s.remote))
	return nil
}
