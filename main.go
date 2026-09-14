package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/Jabberwocky238/go-pop3/pop3"
	"github.com/Jabberwocky238/go-pop3/pop3server"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/responses"
	"github.com/emersion/go-imap/server"
	imap2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message"
	msgtext "github.com/emersion/go-message/textproto"
	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
	"github.com/klauspost/compress/gzip"
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
	StreamWorkers                                          int
	CertFile, KeyFile, TLSDir, JMAPURL                     string
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

const defaultStreamWorkers = 4

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
	var streamWorkers string
	f.StringVar(&streamWorkers, "stream-workers", getenv("STREAM_WORKERS", strconv.Itoa(defaultStreamWorkers)), "parallel MIME ingestion workers (1-128)")
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
	workers, err := strconv.Atoi(streamWorkers)
	if err != nil || workers < 1 || workers > 128 {
		return Config{}, fmt.Errorf("stream-workers must be between 1 and 128")
	}
	c.StreamWorkers = workers
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
	if c.ShowQueue {
		return nil
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
	streamTasks.Close()
	streamTasks = newStreamTaskPool(config.StreamWorkers)
	partProbePool = newBufferPool(config.StreamWorkers, 1<<20)
	partGzipWriters = newWaitPool(config.StreamWorkers, func() *gzip.Writer { w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return w }, func(w *gzip.Writer) { w.Reset(io.Discard) })
	defer streamTasks.Close()
	logger.Info("stream task pool started", "workers", config.StreamWorkers, "queue_capacity", config.StreamWorkers)

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
		if i < 3 {
			l = smtpBufferedListener{Listener: l}
		}
		if i >= 2 && i <= 4 {
			l = tls.NewListener(l, cfg)
		}
		listeners = append(listeners, l)
		closers = append(closers, l)
	}
	var wg sync.WaitGroup
	serveErrors := make(chan error, 10)
	serveGroup := func(jobs ...func() error) {
		defer wg.Done()
		wg.Add(len(jobs))
		for _, job := range jobs {
			go func() { defer wg.Done(); serveErrors <- job() }()
		}
	}
	var jobs []func() error
	for i := 0; i < 3; i++ {
		s := smtp.NewServer(smtpBackend{requireAuth: i != 0})
		s.ErrorLog = slog.NewLogLogger(logger.Handler(), slog.LevelError)
		s.Domain = serverHostname()
		s.TLSConfig = cfg
		s.MaxMessageBytes = maxMailSize
		s.MaxRecipients = 100
		s.ReadTimeout = 5 * time.Minute
		s.WriteTimeout = time.Minute
		closers = append(closers, s)
		l := listeners[i]
		jobs = append(jobs, func() error { return s.Serve(l) })
	}
	im := imapserver.New(&imapserver.Options{TLSConfig: cfg, Logger: slog.NewLogLogger(logger.Handler(), slog.LevelError), Caps: imap2.CapSet{imap2.CapIMAP4rev1: {}, imap2.CapUIDPlus: {}, imap2.CapSpecialUse: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		ctx, cancel := context.WithCancel(ctx)
		return &imapStreamSession{ctx: ctx, cancel: cancel}, nil, nil
	}})
	web := &http.Server{ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError), ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(serveJMAP)}
	pop, pops := newPOPServer(cfg), newPOPServer(cfg)
	closers = append(closers, im, web, pop, pops)
	wg.Add(2)
	jobs = append(jobs, func() error { return serveQueue(ctx) }, func() error { return serveJMAPOwnerFlush(ctx) })
	go serveGroup(jobs...)
	go serveGroup(func() error { return pops.Serve(listeners[3]) }, func() error { return im.Serve(listeners[4]) }, func() error { return web.Serve(listeners[5]) }, func() error { return pop.Serve(listeners[6]) }, func() error { return im.Serve(listeners[7]) })
	logger.Info("SMTP, submission, POP3/STLS, POP3S, IMAP/STARTTLS, IMAPS and HTTP backends ready")
	select {
	case <-ctx.Done():
		err = nil
	case err = <-serveErrors:
	}
	cancel()
	shutdown()
	wg.Wait()
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer flushCancel()
	return errors.Join(err, flushJMAPOwners(flushCtx))
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
	transport.MaxIdleConnsPerHost = 16
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
		case "NoSuchKey", "NoSuchUpload", "NotFound":
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
	namespace string
	store     objectStore
	versions  versionedStore
	mu        sync.Mutex
	record    lockRecord
	lost      error
}

func readLock(store versionedStore, namespace string) (lockRecord, string, error) {
	data, etag, err := store.GetVersion(leaseKey(namespace))
	if err != nil {
		return lockRecord{}, "", err
	}
	var record lockRecord
	if err = json.Unmarshal(data, &record); err != nil || record.Owner == "" || record.StartedAt.IsZero() || record.RenewedAt.IsZero() || record.ExpiresAt.IsZero() {
		return lockRecord{}, "", fmt.Errorf("invalid or legacy .lock; stop the old instance before removing it")
	}
	return record, etag, nil
}
func leaseKey(namespace string) string { return namespace + "/.lock" }
func lockBucket(store objectStore, now time.Time, domain string) (*bucketLease, error) {
	if !validMailDomain(domain) {
		return nil, fmt.Errorf("invalid scanner domain")
	}
	key := leaseKey(domain)
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
	previous, etag, err := readLock(versions, domain)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		err = store.Create(key, data)
	case err != nil:
		return nil, err
	case now.Before(previous.ExpiresAt):
		return nil, fmt.Errorf("scanner locked until %s: %w", previous.ExpiresAt.Format(time.RFC3339Nano), fs.ErrExist)
	default:
		err = versions.Swap(key, data, etag)
	}
	if err != nil {
		return nil, fmt.Errorf("acquire bucket lease: %w", err)
	}
	return &bucketLease{namespace: domain, store: store, versions: versions, record: record}, nil
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
	current, etag, err := readLock(l.versions, l.namespace)
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
	if err := l.versions.Swap(leaseKey(l.namespace), data, etag); err != nil {
		return err
	}
	l.mu.Lock()
	l.record = current
	l.mu.Unlock()
	return nil
}
func (l *bucketLease) release(now time.Time) {
	current, etag, err := readLock(l.versions, l.namespace)
	l.mu.Lock()
	owner := l.record.Owner
	l.mu.Unlock()
	if err != nil || current.Owner != owner {
		return
	}
	current.ExpiresAt = now.UTC()
	data, _ := json.Marshal(current)
	// An expired record remains so release cannot race a new owner via DELETE.
	if err := l.versions.Swap(leaseKey(l.namespace), data, etag); err != nil && !errors.Is(err, fs.ErrExist) {
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

// Domain/account is the only account layout. Protocol usernames are complete
// email addresses; accepting a storage path here would bypass address validation.
var domainLabelRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func validMailDomain(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if !domainLabelRE.MatchString(label) {
			return false
		}
	}
	return true
}

// SMTP identifies the service host, independently of the domains it hosts.
func serverHostname() string {
	if u, err := url.Parse(config.JMAPURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	name, err := os.Hostname()
	if err == nil && validMailDomain(strings.ToLower(name)) {
		return strings.ToLower(name)
	}
	return "localhost"
}
func accountRoot(domain, user string) string { return domain + "/" + user }
func validAccountRoot(root string) bool {
	domain, user, ok := strings.Cut(root, "/")
	return ok && validMailDomain(domain) && usernameRE.MatchString(user)
}
func accountDomain(root string) string {
	domain, _, ok := strings.Cut(root, "/")
	if ok && validMailDomain(domain) {
		return domain
	}
	return ""
}
func accountAddress(root string) string { return path.Base(root) + "@" + accountDomain(root) }
func accountFromKey(key string) string {
	domain, rest, _ := strings.Cut(key, "/")
	user, _, _ := strings.Cut(rest, "/")
	return domain + "/" + user
}
func localUser(address string) string {
	user, domain, ok := strings.Cut(strings.ToLower(address), "@")
	if !ok || !usernameRE.MatchString(user) || !validMailDomain(domain) {
		return ""
	}
	return accountRoot(domain, user)
}

// Check only whether a domain prefix exists, not whether a particular recipient
// exists. Unknown accounts in hosted domains must never fall back to external SMTP.
func domainHasObjects(store objectStore, domain string) (bool, error) {
	if guard, ok := store.(*guardedStore); ok {
		guard.mu.RLock()
		defer guard.mu.RUnlock()
		if guard.closed {
			return false, net.ErrClosed
		}
		return domainHasObjects(guard.base, domain)
	}
	prefix := domain + "/"
	if bucket, ok := store.(*s3Bucket); ok {
		ctx, cancel := bucket.requestContext()
		defer cancel()
		result, err := bucket.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bucket.bucket, Prefix: &prefix, MaxKeys: aws.Int32(1)})
		if err != nil {
			return false, err
		}
		return len(result.Contents) > 0, nil
	}
	keys, err := store.List(prefix)
	return len(keys) > 0, err
}
func deliveryUser(address string) (string, error) {
	_, domain, ok := strings.Cut(strings.ToLower(address), "@")
	if !ok || !validMailDomain(domain) {
		return "", nil
	}
	local, err := domainHasObjects(objects, domain)
	if err != nil {
		return "", err
	}
	if !local {
		return "", nil
	}
	root := localUser(address)
	if root == "" {
		return "", fs.ErrNotExist
	}
	return root, nil
}

type identityKind string

const (
	kindAccount identityKind = "account"
	kindAlias   identityKind = "alias"
	kindProxy   identityKind = "proxy"
)

func readKind(id string) (identityKind, error) {
	if !validAccountRoot(id) {
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
	return accountRoot(accountDomain(id), target), nil
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
	if !validAccountRoot(user) {
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

// Coalesce the protocol library's small reads before crossing into the kernel.
// Wrap TCP below TLS so implicit TLS detection and STARTTLS remain intact.
type smtpBufferedListener struct{ net.Listener }

func (l smtpBufferedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &smtpBufferedConn{Conn: c, reader: bufio.NewReaderSize(c, 1<<20)}, nil
}

type smtpBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *smtpBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

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
			next, routeErr := deliveryUser(target)
			if routeErr != nil {
				return "", "", "", routeErr
			}
			if next == "" {
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
	u, routeErr := deliveryUser(to)
	if routeErr != nil {
		if errors.Is(routeErr, fs.ErrNotExist) {
			return &smtp.SMTPError{Code: 550, Message: "unknown local recipient"}
		}
		return &smtp.SMTPError{Code: 451, Message: "domain storage unavailable"}
	}
	if u == "" {
		address, err := mail.ParseAddress(to)
		if err != nil || address.Address != to || !strings.Contains(to, "@") || strings.ContainsAny(to, "\r\n") {
			return &smtp.SMTPError{Code: 553, Message: "invalid recipient"}
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
	refs    map[uint32]jmapBlobRef
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
	refs, err := mailBlobRefs(ctx, user)
	if err != nil {
		lock.Unlock()
		return err
	}
	p.refs = refs
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
		b, otherErr := resolveIdentity(accountAddress(user))
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
		return []pop3.MessageInfo{{Num: n, Size: storedMessageSize(m, p.refs)}}, nil
	}
	var result []pop3.MessageInfo
	for i, m := range p.msgs {
		if !p.deleted[i+1] {
			result = append(result, pop3.MessageInfo{Num: i + 1, Size: storedMessageSize(m, p.refs)})
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
	r, _, err := openStoredMessage(ctx, m, p.refs)
	return r, err
}
func (p *popMailbox) Top(ctx context.Context, n, lines int) (io.ReadCloser, error) {
	m, err := p.message(ctx, n)
	if err != nil {
		return nil, err
	}
	if lines < 0 {
		return nil, fmt.Errorf("invalid line count")
	}
	r, _, err := openStoredMessage(ctx, m, p.refs)
	if err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	go func() {
		defer r.Close()
		buffer := bufio.NewReader(r)
		inBody, fragment := false, false
		for !inBody || lines > 0 {
			line, e := buffer.ReadSlice('\n')
			if _, err := writer.Write(line); err != nil {
				writer.CloseWithError(err)
				return
			}
			if e != bufio.ErrBufferFull {
				if inBody {
					lines--
				} else if !fragment && len(bytes.TrimRight(line, "\r\n")) == 0 {
					inBody = true
				}
			}
			fragment = e == bufio.ErrBufferFull
			if e != nil && e != bufio.ErrBufferFull {
				if e == io.EOF {
					e = nil
				}
				writer.CloseWithError(e)
				return
			}
		}
		writer.Close()
	}()
	return reader, nil
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

// Discover hosted domains from bucket prefixes on each scheduler sweep.
func domainNamespaces() ([]string, error) {
	prefixes, err := storePrefixes(objects, "")
	if err != nil {
		return nil, err
	}
	var domains []string
	for _, prefix := range prefixes {
		domain := strings.TrimSuffix(prefix, "/")
		if validMailDomain(domain) {
			domains = append(domains, domain)
		}
	}
	return domains, nil
}
func queueDirectory(root string) string { return path.Join(accountDomain(root), outbox) }

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
	owner := user
	if owner == "" && len(proxies) > 0 {
		owner = proxies[0].Owner
	}
	if owner == "" && len(local) > 0 {
		owner = local[0]
	}
	if err := writeJSON(path.Join(queueDirectory(owner), job.ID+".json"), job); err != nil {
		return err
	}
	logger.Info(fmt.Sprintf("outbound queued id=%s recipients=%d", job.ID, len(remote)))
	return nil
}

func listQueue() error {
	namespaces, err := domainNamespaces()
	if err != nil {
		return err
	}
	for _, namespace := range namespaces {
		err := eachJSON(path.Join(namespace, outbox), func(_ string, job *outboundJob, err error) error {
			if err != nil {
				return err
			}
			for _, r := range job.Recipients {
				fmt.Printf("%s %s %s attempts=%d next=%s error=%q\n", path.Join(namespace, job.ID), r.Address, r.State, r.Attempts, r.Next.Format(time.RFC3339), r.Error)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
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
			notice := fmt.Sprintf("From: mailer-daemon@%s\r\nTo: %s@%s\r\nDate: %s\r\nMessage-ID: <%s-failure@mail.%s>\r\nSubject: Delivery failed [%s]\r\nContent-Type: text/plain; charset=utf-8\r\nAuto-Submitted: auto-replied\r\n\r\nOutbound delivery failed. Queue ID: %s\r\n%s\r\n", accountDomain(user), path.Base(user), accountDomain(user), time.Now().Format(time.RFC1123Z), job.ID, accountDomain(user), job.ID, job.ID, strings.Join(failed, "\r\n"))
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
	directory := path.Join(lease.namespace, outbox)
	keys, err := objects.List(directory + "/")
	if err != nil {
		return false, err
	}
	claimed := 0
	for _, key := range keys {
		if ctx.Err() != nil || !lease.valid(time.Now()) {
			break
		}
		if path.Dir(key) != directory || !strings.HasSuffix(key, ".json") {
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
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return false, ctx.Err()
		}
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
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	active := map[string]context.CancelFunc{}
	defer func() { cancel(); workers.Wait() }()
	ticker := time.NewTicker(taskScanEvery)
	defer ticker.Stop()
	failures := make(chan error, 1)
	slots := make(chan struct{}, maxClaimedTasks)
	for {
		domains, err := domainNamespaces()
		if err != nil {
			return fmt.Errorf("discover mail domains: %w", err)
		}
		for domain, stop := range active {
			if !slices.Contains(domains, domain) {
				stop()
				delete(active, domain)
			}
		}
		for _, domain := range domains {
			if _, exists := active[domain]; exists {
				continue
			}
			domainCtx, stop := context.WithCancel(ctx)
			active[domain] = stop
			workers.Add(1)
			go func() {
				defer workers.Done()
				if err := serveDomainQueue(domainCtx, domain, slots); err != nil {
					select {
					case failures <- err:
					default:
					}
				}
			}()
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case <-ticker.C:
		}
	}
}
func serveDomainQueue(ctx context.Context, namespace string, slots chan struct{}) error {
	ticker := time.NewTicker(taskScanEvery)
	defer ticker.Stop()
	var workers sync.WaitGroup
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
				lease, err = lockBucket(objects, time.Now(), namespace)
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
			notifyIMAP()
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

// DKIM keys are selected per tenant. Publishing a new key before changing the
// selector makes rotation atomic; queued messages never contain private keys.
func (job *outboundJob) openSignedBody(ctx context.Context) (io.ReadCloser, error) {
	if job.User == "" {
		return job.openBody(ctx)
	}
	if !validAccountRoot(job.User) {
		return nil, errors.New("invalid DKIM account")
	}
	domain := accountDomain(job.User)
	selectorBytes, err := objects.Get(domain + "/.dkim/selector")
	if errors.Is(err, fs.ErrNotExist) {
		return job.openBody(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("DKIM selector: %w", err)
	}
	selector := strings.TrimSpace(string(selectorBytes))
	if !validMailDomain(selector) || len(selector+"._domainkey."+domain) > 253 {
		return nil, errors.New("invalid DKIM selector")
	}
	keyBytes, err := objects.Get(domain + "/.dkim/" + selector + ".pem")
	if err != nil {
		return nil, fmt.Errorf("DKIM key: %w", err)
	}
	if len(keyBytes) > 16384 {
		return nil, errors.New("DKIM key too large")
	}
	block, rest := pem.Decode(keyBytes)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid DKIM PEM")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		key, _ = parsed.(*rsa.PrivateKey)
	default:
		err = errors.New("unsupported DKIM PEM type")
	}
	if err != nil || key == nil {
		return nil, errors.New("DKIM requires an RSA private key")
	}
	if key.N.BitLen() < 2048 {
		return nil, errors.New("DKIM RSA key must be at least 2048 bits")
	}
	if err := key.Validate(); err != nil {
		return nil, errors.New("invalid DKIM RSA key")
	}
	body, err := job.openBody(ctx)
	if err != nil {
		return nil, err
	}
	scanBody := body
	stop := context.AfterFunc(ctx, func() { scanBody.Close() })
	header, err := dkimSignature(ctx, body, job.User, selector, key)
	stop()
	body.Close()
	if err != nil {
		return nil, fmt.Errorf("DKIM signing: %w", err)
	}
	body, err = job.openBody(ctx)
	if err != nil {
		return nil, err
	}
	return &prefixedReader{Reader: io.MultiReader(strings.NewReader(header), body), closer: body}, nil
}

// Relaxed canonicalization only folds ASCII whitespace, not Unicode spaces.
func dkimRelaxed(value string) string {
	return strings.Join(strings.FieldsFunc(value, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\r' || r == '\n'
	}), " ")
}

func dkimSignature(ctx context.Context, source io.Reader, user, selector string, key *rsa.PrivateKey) (string, error) {
	reader := bufio.NewReader(source)
	var raw bytes.Buffer
	lineSize := 0
	for {
		line, err := reader.ReadSlice('\n')
		if raw.Len()+len(line) > 64*1024 {
			return "", errors.New("DKIM headers exceed 64 KiB")
		}
		raw.Write(line)
		lineSize += len(line)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return "", err
		}
		if lineSize == 2 && bytes.Equal(line, []byte("\r\n")) || lineSize == 1 && bytes.Equal(line, []byte("\n")) {
			break
		}
		lineSize = 0
	}
	headers, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw.Bytes()))).ReadMIMEHeader()
	if err != nil {
		return "", err
	}
	from := headers.Values("From")
	if len(from) != 1 {
		return "", errors.New("DKIM requires exactly one From header")
	}
	addresses, err := mail.ParseAddressList(from[0])
	if err != nil || len(addresses) != 1 {
		return "", errors.New("DKIM requires exactly one From address")
	}
	identity, err := resolveIdentity(addresses[0].Address)
	if err != nil || identity.RootID != user || accountDomain(localUser(addresses[0].Address)) != accountDomain(user) {
		return "", errors.New("DKIM From does not belong to the authenticated account")
	}
	bodyHash, err := dkimBodyHash(ctx, reader)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	var names []string
	for _, name := range []string{"From", "Sender", "Reply-To", "To", "Cc", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type", "Content-Transfer-Encoding", "In-Reply-To", "References"} {
		values := headers.Values(name)
		for i := len(values) - 1; i >= 0; i-- {
			fmt.Fprintf(h, "%s:%s\r\n", strings.ToLower(name), dkimRelaxed(values[i]))
			names = append(names, strings.ToLower(name))
		}
		// Oversign one missing occurrence to detect inserted headers as well.
		names = append(names, strings.ToLower(name))
	}
	value := "v=1; a=rsa-sha256; c=relaxed/relaxed;\r\n\td=" + accountDomain(user) + "; s=" + selector + ";\r\n\th=" + strings.Join(names, ":\r\n\t") + ";\r\n\tbh=" + base64.StdEncoding.EncodeToString(bodyHash) + ";\r\n\tb="
	fmt.Fprintf(h, "dkim-signature:%s", dkimRelaxed(value))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h.Sum(nil))
	if err != nil {
		return "", err
	}
	encoded := base64.StdEncoding.EncodeToString(sig)
	for len(encoded) > 64 {
		value += encoded[:64] + "\r\n\t"
		encoded = encoded[64:]
	}
	return "DKIM-Signature: " + value + encoded + "\r\n", nil
}

// Keep only a count of pending empty lines, so even a body consisting entirely
// of CRLFs uses fixed memory. SMTP's DotWriter normalizes bare LF to CRLF too.
func dkimBodyHash(ctx context.Context, source io.Reader) ([]byte, error) {
	h := sha256.New()
	out := bufio.NewWriter(h)
	reader := bufio.NewReader(source)
	var emptyLines int64
	var whitespace, pendingCR, emitted bool
	flushLines := func() {
		for emptyLines > 0 {
			out.WriteString("\r\n")
			emptyLines--
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := reader.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if pendingCR && b != '\n' {
			return nil, errors.New("bare CR in DKIM body")
		}
		pendingCR = false
		switch b {
		case '\r':
			pendingCR = true
		case '\n':
			emptyLines++
			whitespace = false
		case ' ', '\t':
			whitespace = true
		default:
			flushLines()
			if whitespace {
				out.WriteByte(' ')
			}
			out.WriteByte(b)
			whitespace, emitted = false, true
		}
	}
	if pendingCR {
		return nil, errors.New("bare CR in DKIM body")
	}
	if emitted {
		out.WriteString("\r\n")
	}
	out.Flush()
	return h.Sum(nil), nil
}

func sendSMTP(ctx context.Context, address string, job *outboundJob, recipient string, relay bool) error {
	body, err := job.openSignedBody(ctx)
	if err != nil {
		return err
	}
	defer body.Close()
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
	if err = client.Hello(serverHostname()); err != nil {
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
	if _, err = copyStream(ctx, writer, body); err != nil {
		return err
	}
	// Only the final DATA response confirms acceptance; QUIT failure must not resend it.
	if err = writer.Close(); err != nil {
		return err
	}
	return nil
}

// Owner records are authoritative; the account file holds only user data and
// the current conditional-write decision. Query indexes exist only in memory.
type jmapOwnerFile struct {
	Format      int                `json:"format"`
	User        map[string][]byte  `json:"user"`
	Transaction string             `json:"transaction,omitempty"`
	Pending     []jmapOwnerPending `json:"pending,omitempty"`
}
type jmapOwnerPending struct {
	Key      string `json:"key"`
	Prepared string `json:"prepared"`
	Previous string `json:"previous,omitempty"`
}
type jmapOwnerRecord struct {
	Format      int             `json:"format"`
	Key         string          `json:"key"`
	Transaction string          `json:"transaction"`
	Record      json.RawMessage `json:"record,omitempty"`
	Value       []byte          `json:"value,omitempty"`
	Deleted     bool            `json:"deleted,omitempty"`
}

func (r jmapOwnerRecord) bytes() []byte {
	if r.Record != nil {
		return bytes.Clone(r.Record)
	}
	if r.Value == nil {
		return []byte{}
	}
	return bytes.Clone(r.Value)
}

type jmapOwnerState struct {
	mu        sync.Mutex
	key       string
	store     objectStore
	loaded    bool
	data      map[string][]byte
	locations map[string]string
	ordered   []string
	head      jmapOwnerFile
	etag      string
	finalized string
	checked   time.Time
	rebuild   func(map[string][]byte) error
}

type jmapOwnerCacheKey struct {
	store objectStore
	key   string
}

var jmapOwnerStates = struct {
	sync.Mutex
	states map[jmapOwnerCacheKey]*jmapOwnerState
}{states: map[jmapOwnerCacheKey]*jmapOwnerState{}}

type jmapBackend struct {
	key    string
	store  objectStore
	mu     sync.Mutex
	closed bool
	state  *jmapOwnerState
}

func (b *jmapBackend) owner() (*jmapOwnerState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, fs.ErrClosed
	}
	if b.state == nil {
		k := jmapOwnerCacheKey{b.store, b.key}
		jmapOwnerStates.Lock()
		b.state = jmapOwnerStates.states[k]
		if b.state == nil {
			b.state = &jmapOwnerState{key: b.key, store: b.store}
			jmapOwnerStates.states[k] = b.state
		}
		jmapOwnerStates.Unlock()
	}
	return b.state, nil
}
func jmapKeySegments(k []byte) []string {
	var out []string
	var part []byte
	for i := 0; i < len(k); i++ {
		if k[i] != 0 {
			part = append(part, k[i])
			continue
		}
		i++
		if i == len(k) {
			return nil
		}
		switch k[i] {
		case 255:
			part = append(part, 0)
		case 1:
			out = append(out, string(part))
			part = nil
		default:
			return nil
		}
	}
	if len(part) != 0 {
		return nil
	}
	return out
}

// Encode the upstream backend's ordered, escaped key segments.
func jmapIndexKey(parts ...string) []byte {
	var key []byte
	for _, part := range parts {
		for i := 0; i < len(part); i++ {
			key = append(key, part[i])
			if part[i] == 0 {
				key = append(key, 255)
			}
		}
		key = append(key, 0, 1)
	}
	return key
}

// Rebuild derived rows using registered descriptors and the library's public
// sort codec, keeping ordering identical to live objectdb writes.
func jmapObjectIndexes(t *jdescriptor.Type, account, id jmap.Id, raw []byte) (*jbackend.Batch, error) {
	if t == nil {
		return nil, fmt.Errorf("unknown stored object type")
	}
	var obj jdb.Object
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	batch := &jbackend.Batch{}
	for name, prop := range t.Properties {
		value, exists := obj[name]
		if !exists {
			continue
		}
		if prop.Indexed {
			v, err := jdb.SortKey(prop, value)
			if err != nil {
				return nil, err
			}
			parts := []string{string(account), "x", t.Name, name, string(v)}
			for _, sibling := range prop.OrderBy {
				var order []byte
				if r, ok := obj[sibling]; ok {
					order, err = jdb.SortKey(t.Properties[sibling], r)
					if err != nil {
						return nil, err
					}
				}
				parts = append(parts, string(order))
			}
			batch.Set(jmapIndexKey(append(parts, string(id))...), nil)
		}
		if prop.SetIndexed {
			var members []string
			switch prop.Kind {
			case jdescriptor.KindObject:
				var set map[string]json.RawMessage
				if err := json.Unmarshal(value, &set); err != nil {
					return nil, err
				}
				for member := range set {
					members = append(members, member)
				}
			case jdescriptor.KindArray:
				if err := json.Unmarshal(value, &members); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("invalid set index kind")
			}
			for _, member := range members {
				batch.Set(jmapIndexKey(string(account), "x", t.Name, name, member, string(id)), nil)
			}
		}
		if prop.BlobRef && string(value) != "null" {
			var blob string
			if err := json.Unmarshal(value, &blob); err != nil {
				return nil, err
			}
			batch.Set(jmapIndexKey(string(account), "r", blob, t.Name, string(id)), nil)
		}
	}
	return batch, nil
}

func jmapTransientKey(k []byte) bool {
	parts := jmapKeySegments(k)
	if len(parts) > 0 && parts[0] == "!tag" {
		return true
	}
	return len(parts) > 1 && (parts[1] == "x" || parts[1] == "r" || parts[1] == "p")
}
func jmapOwnedKey(k []byte) bool {
	p := jmapKeySegments(k)
	return len(p) > 1 && (p[1] == "g" || p[1] == "u" || (p[1] == "o" && len(p) > 2 && (p[2] == "Email" || p[2] == "Thread" || p[2] == "EmailSubmission")))
}
func (s *jmapOwnerState) root() string { return strings.TrimSuffix(s.key, "/.jmap/state.json") }
func (s *jmapOwnerState) recordPath(k string, v []byte, delta map[string]*[]byte) (string, error) {
	if existing := s.locations[k]; existing != "" {
		return existing, nil
	}
	raw, err := hex.DecodeString(k)
	if err != nil {
		return "", err
	}
	parts := jmapKeySegments(raw)
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid owned record key")
	}
	root := s.root()
	parentForBlob := func(id jmap.Id) (string, error) {
		key, e := (jmapBlobs{store: s.store}).key(jmapAccountID(root), id)
		if e != nil {
			return "", e
		}
		if jmapBlobDescriptorID(key) != "" {
			return path.Dir(key), nil
		}
		data, e := s.store.Get(key)
		if e != nil {
			return "", e
		}
		ref, e := decodeObjectReference(key, data)
		if e != nil {
			return root + "/mail/legacy-" + string(id), nil
		}
		return path.Dir(ref.Key), nil
	}
	if parts[1] == "o" && len(parts) == 4 && parts[2] == "Email" {
		var obj jdb.Object
		if err = json.Unmarshal(v, &obj); err != nil {
			return "", err
		}
		parent, e := parentForBlob(jvalue[jmap.Id](obj, "blobId"))
		if e != nil {
			return "", e
		}
		return parent + "/.fma/email-" + parts[3] + ".fma.json", nil
	}
	if parts[1] == "u" && len(parts) == 3 {
		parent, e := parentForBlob(jmap.Id(parts[2]))
		if errors.Is(e, fs.ErrNotExist) {
			// The library records an upload before publishing/copying its bytes.
			parent, e = root+"/mail/.uploads/"+parts[2], nil
		}
		if e != nil {
			return "", e
		}
		return parent + "/.fma/upload-" + parts[2] + ".fma.json", nil
	}
	if parts[1] == "g" {
		// A single-email change belongs beside that email. Mixed/account changes
		// remain account history, not a lookup index.
		var emailKey string
		var emailValue []byte
		count := 0
		for key, value := range delta {
			decoded, _ := hex.DecodeString(key)
			p := jmapKeySegments(decoded)
			if len(p) == 4 && p[1] == "o" && p[2] == "Email" {
				count++
				emailKey = key
				if value != nil {
					emailValue = *value
				} else {
					emailValue = s.data[key]
				}
			}
		}
		if count == 1 {
			owner, e := s.recordPath(emailKey, emailValue, nil)
			if e != nil {
				return "", e
			}
			return path.Dir(owner) + "/change-" + k + ".fma.json", nil
		}
		return root + "/mail/.history/" + k + ".fma.json", nil
	}
	if len(parts) == 4 && parts[1] == "o" {
		return root + "/mail/.records/" + parts[2] + "/" + parts[3] + ".fma.json", nil
	}
	return "", fmt.Errorf("unhandled owned metadata key")
}
func (s *jmapOwnerState) applyRecord(data []byte, location string) error {
	var r jmapOwnerRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	raw, err := hex.DecodeString(r.Key)
	if err != nil || r.Format != 1 || !jmapOwnedKey(raw) {
		return fmt.Errorf("invalid owner record: %s", location)
	}
	s.locations[r.Key] = location
	if r.Deleted {
		delete(s.data, r.Key)
	} else {
		s.data[r.Key] = r.bytes()
	}
	return nil
}
func (s *jmapOwnerState) refresh(ctx context.Context, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.loaded && !force && time.Since(s.checked) < time.Second {
		return nil
	}
	raw, etag, err := s.store.(versionedStore).GetVersion(s.key)
	if errors.Is(err, fs.ErrNotExist) {
		raw = nil
		etag = ""
	} else if err != nil {
		return err
	}
	if s.loaded && etag == s.etag {
		s.checked = time.Now()
		return nil
	}
	var head jmapOwnerFile
	if raw != nil {
		if err = json.Unmarshal(raw, &head); err != nil {
			return err
		}
	}
	if head.Format != 1 {
		// Legacy bootstrap and upgrade: read the old image once. Migration below
		// writes owner records before replacing the account file conditionally.
		state := map[string][]byte{}
		if raw != nil {
			if err = json.Unmarshal(raw, &state); err != nil {
				return err
			}
		}
		s.data = state
		s.ordered = nil
		s.locations = map[string]string{}
		s.etag = etag
		s.head = jmapOwnerFile{Format: 1, User: map[string][]byte{}}
		s.loaded = true
		delta := map[string]*[]byte{}
		for k, v := range state {
			key, _ := hex.DecodeString(k)
			if jmapTransientKey(key) {
				continue
			}
			copy := bytes.Clone(v)
			delta[k] = &copy
		}
		if err = s.persist(ctx, delta); err != nil {
			s.loaded = false
			return err
		}
		s.checked = time.Now()
		return nil
	}
	state := map[string][]byte{}
	for k, v := range head.User {
		key, e := hex.DecodeString(k)
		if e != nil || jmapTransientKey(key) || jmapOwnedKey(key) {
			return fmt.Errorf("invalid account user record")
		}
		state[k] = bytes.Clone(v)
	}
	oldData, oldLocations := s.data, s.locations
	s.data = state
	s.ordered = nil
	s.locations = map[string]string{}
	success := false
	defer func() {
		if !success {
			s.data = oldData
			s.locations = oldLocations
		}
	}()
	keys, err := s.store.List(s.root() + "/")
	if err != nil {
		return err
	}
	for _, key := range keys {
		if id := jmapBlobDescriptorID(key); id != "" {
			(jmapBlobs{store: s.store}).remember(s.root(), jmap.Id(id), key)
		}
		if !strings.HasSuffix(key, ".fma.json") {
			continue
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		data, e := s.store.Get(key)
		if e != nil {
			return e
		}
		if e = s.applyRecord(data, key); e != nil {
			return e
		}
	}
	for _, pending := range head.Pending {
		if !strings.HasPrefix(pending.Key, s.root()+"/") || pending.Prepared != pending.Key+".prepare."+head.Transaction {
			return fmt.Errorf("invalid metadata commit owner")
		}
		data, e := s.store.Get(pending.Prepared)
		if e != nil {
			return e
		}
		if e = s.applyRecord(data, pending.Key); e != nil {
			return e
		}
	}
	// Do not combine records from different commits while rebuilding.
	_, current, e := s.store.(versionedStore).GetVersion(s.key)
	if e != nil {
		return e
	}
	if current != etag {
		return jbackend.ErrAssertFailed
	}
	if s.rebuild != nil {
		if err = s.rebuild(s.data); err != nil {
			return err
		}
	}
	s.head = head
	s.etag = etag
	s.loaded = true
	s.checked = time.Now()
	success = true
	return nil
}
func (s *jmapOwnerState) flush(ctx context.Context) error {
	if s.finalized == s.head.Transaction {
		return nil
	}
	for _, p := range s.head.Pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := s.store.Get(p.Prepared)
		if err != nil {
			return err
		}
		old, etag, err := s.store.(versionedStore).GetVersion(p.Key)
		if err == nil {
			var record jmapOwnerRecord
			if json.Unmarshal(old, &record) == nil && record.Transaction == s.head.Transaction {
				continue
			}
			if etag != p.Previous {
				return fmt.Errorf("owner record changed before flush: %s: %w", p.Key, fs.ErrExist)
			}
			err = s.store.(versionedStore).Swap(p.Key, data, etag)
		} else if errors.Is(err, fs.ErrNotExist) && p.Previous == "" {
			err = s.store.Create(p.Key, data)
		}
		if err != nil {
			return err
		}
	}
	s.finalized = s.head.Transaction
	return nil
}
func (s *jmapOwnerState) persist(ctx context.Context, delta map[string]*[]byte) error {
	durable := false
	for key, value := range delta {
		raw, err := hex.DecodeString(key)
		if err != nil {
			return err
		}
		old, exists := s.data[key]
		if !jmapTransientKey(raw) && ((value == nil && exists) || (value != nil && (!exists || !bytes.Equal(old, *value)))) {
			durable = true
		}
	}
	if !durable && s.etag != "" && s.head.Transaction != "" {
		for key, value := range delta {
			_, exists := s.data[key]
			if exists != (value != nil) {
				s.ordered = nil
			}
			if value == nil {
				delete(s.data, key)
			} else {
				s.data[key] = bytes.Clone(*value)
			}
		}
		return nil
	}
	if err := s.flush(ctx); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tx := hex.EncodeToString(nonce[:])
	next := jmapOwnerFile{Format: 1, User: maps.Clone(s.head.User), Transaction: tx}
	if next.User == nil {
		next.User = map[string][]byte{}
	}
	locations := map[string]string{}
	for k, value := range delta {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, e := hex.DecodeString(k)
		if e != nil {
			return e
		}
		if jmapTransientKey(key) {
			continue
		}
		if s.head.Transaction != "" {
			old, exists := s.data[k]
			if (value == nil && !exists) || (value != nil && exists && bytes.Equal(old, *value)) {
				continue
			}
		}
		if !jmapOwnedKey(key) {
			if value == nil {
				delete(next.User, k)
			} else {
				next.User[k] = bytes.Clone(*value)
			}
			continue
		}
		v := s.data[k]
		if value != nil {
			v = *value
		}
		location, e := s.recordPath(k, v, delta)
		if e != nil {
			return e
		}
		record := jmapOwnerRecord{Format: 1, Key: k, Transaction: tx, Deleted: value == nil}
		if value != nil {
			if json.Valid(*value) {
				record.Record = bytes.Clone(*value)
			} else {
				record.Value = bytes.Clone(*value)
			}
		}
		data, e := json.Marshal(record)
		if e != nil {
			return e
		}
		prepared := location + ".prepare." + tx
		_, previous, e := s.store.(versionedStore).GetVersion(location)
		if e != nil && !errors.Is(e, fs.ErrNotExist) {
			return e
		}
		if e = s.store.Create(prepared, data); e != nil {
			return e
		}
		next.Pending = append(next.Pending, jmapOwnerPending{Key: location, Prepared: prepared, Previous: previous})
		locations[k] = location
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if s.etag == "" {
		err = s.store.Create(s.key, data)
	} else {
		err = s.store.(versionedStore).Swap(s.key, data, s.etag)
	}
	if err != nil {
		return err
	}
	// Reading back is only for the ETag; a concurrent commit is detected on the
	// next refresh rather than assigning its version to our local snapshot.
	latest, etag, e := s.store.(versionedStore).GetVersion(s.key)
	if e != nil || !bytes.Equal(latest, data) {
		s.loaded = false
	} else {
		s.etag = etag
	}
	s.head = next
	s.finalized = ""
	s.checked = time.Now()
	var added []string
	removed := false
	for k, v := range delta {
		if _, exists := s.data[k]; !exists && v != nil {
			added = append(added, k)
		}
		if v == nil {
			if _, exists := s.data[k]; exists {
				removed = true
			}
			delete(s.data, k)
		} else {
			s.data[k] = bytes.Clone(*v)
		}
	}
	if s.ordered != nil && (len(added) > 0 || removed) {
		sort.Strings(added)
		merged := make([]string, 0, len(s.ordered)+len(added))
		i := 0
		for _, key := range s.ordered {
			if _, exists := s.data[key]; !exists {
				continue
			}
			for i < len(added) && added[i] < key {
				merged = append(merged, added[i])
				i++
			}
			merged = append(merged, key)
		}
		s.ordered = append(merged, added[i:]...)
	}
	for k, v := range locations {
		s.locations[k] = v
	}
	return nil
}
func (b *jmapBackend) Get(ctx context.Context, key []byte) ([]byte, error) {
	s, err := b.owner()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.refresh(ctx, false); err != nil {
		return nil, err
	}
	value, ok := s.data[hex.EncodeToString(key)]
	if !ok {
		return nil, jbackend.ErrNotFound
	}
	return bytes.Clone(value), nil
}
func (b *jmapBackend) MultiGet(ctx context.Context, keys [][]byte) ([][]byte, error) {
	s, err := b.owner()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.refresh(ctx, false); err != nil {
		return nil, err
	}
	out := make([][]byte, len(keys))
	for i, k := range keys {
		if v, ok := s.data[hex.EncodeToString(k)]; ok {
			out[i] = append([]byte{}, v...)
		}
	}
	return out, nil
}
func (b *jmapBackend) Scan(ctx context.Context, start, end []byte, reverse bool, visit func([]byte, []byte) bool) error {
	s, err := b.owner()
	if err != nil {
		return err
	}
	s.mu.Lock()
	if err = s.refresh(ctx, false); err != nil {
		s.mu.Unlock()
		return err
	}
	type item struct{ k, v []byte }
	var values []item
	if s.ordered == nil {
		s.ordered = slices.Sorted(maps.Keys(s.data))
	}
	low, _ := slices.BinarySearch(s.ordered, hex.EncodeToString(start))
	high := len(s.ordered)
	if end != nil {
		high, _ = slices.BinarySearch(s.ordered, hex.EncodeToString(end))
	}
	if high < low {
		high = low
	}
	for _, encoded := range s.ordered[low:high] {
		key, e := hex.DecodeString(encoded)
		if e != nil {
			s.mu.Unlock()
			return e
		}
		values = append(values, item{key, bytes.Clone(s.data[encoded])})
	}
	s.mu.Unlock()
	if reverse {
		slices.Reverse(values)
	}
	for _, v := range values {
		if err = ctx.Err(); err != nil {
			return err
		}
		if !visit(v.k, v.v) {
			break
		}
	}
	return nil
}
func (b *jmapBackend) WriteBatch(ctx context.Context, batch *jbackend.Batch) error {
	s, err := b.owner()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.refresh(ctx, true); err != nil {
		return err
	}
	for _, op := range batch.Ops {
		if op.Kind != jbackend.OpAssert {
			continue
		}
		v, ok := s.data[hex.EncodeToString(op.Key)]
		if (op.Value == nil && ok) || (op.Value != nil && (!ok || !bytes.Equal(v, op.Value))) {
			return jbackend.ErrAssertFailed
		}
	}
	delta := map[string]*[]byte{}
	for _, op := range batch.Ops {
		k := hex.EncodeToString(op.Key)
		switch op.Kind {
		case jbackend.OpSet:
			v := bytes.Clone(op.Value)
			if bytes.Contains(op.Key, []byte("\x00\x01o\x00\x01Email\x00\x01")) {
				var email jdb.Object
				if json.Unmarshal(v, &email) == nil {
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
						v = jraw(email)
					}
				}
			}
			delta[k] = &v
		case jbackend.OpDelete:
			delta[k] = nil
		case jbackend.OpAdd:
			old := s.data[k]
			if v, ok := delta[k]; ok {
				old = nil
				if v != nil {
					old = *v
				}
			}
			var n int64
			if old != nil {
				n, err = jbackend.DecodeInt64(old)
				if err != nil {
					return err
				}
			}
			v := jbackend.EncodeInt64(n + op.Delta)
			delta[k] = &v
		case jbackend.OpAssert:
		default:
			return fmt.Errorf("invalid JMAP batch operation")
		}
	}
	if len(delta) == 0 {
		return nil
	}
	err = s.persist(ctx, delta)
	if errors.Is(err, fs.ErrExist) {
		s.loaded = false
		return jbackend.ErrAssertFailed
	}
	if err == nil {
		notifyIMAP()
	}
	return err
}
func (b *jmapBackend) Close() error { b.mu.Lock(); b.closed = true; b.mu.Unlock(); return nil }

// JMAP's ordered key/value records for one account commit in one conditional
// S3 write. Binary MIME blobs live separately, so transactions copy metadata only.
// There is no local database and no process-owned lease or background maintainer.
type jmapLegacyBackend struct {
	key    string
	store  objectStore
	mu     sync.RWMutex
	closed bool
}

func (b *jmapLegacyBackend) snapshot(ctx context.Context) (map[string][]byte, string, error) {
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
func (b *jmapLegacyBackend) Get(ctx context.Context, key []byte) ([]byte, error) {
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
func (b *jmapLegacyBackend) Scan(ctx context.Context, start, end []byte, reverse bool, visit func([]byte, []byte) bool) error {
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
func (b *jmapLegacyBackend) WriteBatch(ctx context.Context, batch *jbackend.Batch) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return fs.ErrClosed
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
			if err == nil {
				notifyIMAP()
			}
			return err
		}
	}
	return fmt.Errorf("JMAP metadata contention: %s", b.key)
}
func (b *jmapLegacyBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

type jmapBlobs struct{ store objectStore }

var jmapBlobIDRE = regexp.MustCompile(`^G[A-Za-z0-9_-]{43}$`)

func jmapAccountID(root string) jmap.Id { return jmap.Id("A" + hex.EncodeToString([]byte(root))) }
func jmapRoot(acct jmap.Id) (string, error) {
	if !strings.HasPrefix(string(acct), "A") {
		return "", jauth.ErrUnauthenticated
	}
	data, err := hex.DecodeString(string(acct)[1:])
	root := string(data)
	if err != nil || !validAccountRoot(root) {
		return "", jauth.ErrUnauthenticated
	}
	return root, nil
}

// Blob identity/encoding descriptors live beside their bytes. The reverse
// lookup from content ID to descriptor is rebuilt from S3 names, never saved.
type jmapBlobDirectory struct {
	mu      sync.Mutex
	paths   map[string]string
	parts   map[jmap.Id]jmapPartManifest
	checked time.Time
}

var jmapBlobDirectories sync.Map

func jmapBaseStore(store objectStore) objectStore {
	if staged, ok := store.(*jmapBootstrapStore); ok {
		return staged.objectStore
	}
	return store
}
func (s jmapBlobs) directory(root string) *jmapBlobDirectory {
	key := jmapOwnerCacheKey{jmapBaseStore(s.store), root}
	value, _ := jmapBlobDirectories.LoadOrStore(key, &jmapBlobDirectory{paths: map[string]string{}, parts: map[jmap.Id]jmapPartManifest{}})
	return value.(*jmapBlobDirectory)
}
func (s jmapBlobs) remember(root string, id jmap.Id, key string) {
	d := s.directory(root)
	d.mu.Lock()
	d.paths[string(id)] = key
	d.mu.Unlock()
}
func jmapBlobDescriptorID(key string) string {
	name := path.Base(key)
	at := strings.LastIndex(name, ".blob-")
	if at < 0 {
		return ""
	}
	id := name[at+len(".blob-"):]
	if !strings.HasSuffix(id, ".json") {
		return ""
	}
	id = strings.TrimSuffix(id, ".json")
	if !jmapBlobIDRE.MatchString(id) {
		return ""
	}
	return id
}
func (s jmapBlobs) key(acct, id jmap.Id) (string, error) {
	root, err := jmapRoot(acct)
	if err != nil {
		return "", err
	}
	if !jmapBlobIDRE.MatchString(string(id)) {
		return "", jblob.ErrNotFound
	}
	d := s.directory(root)
	d.mu.Lock()
	defer d.mu.Unlock()
	if key := d.paths[string(id)]; key != "" {
		return key, nil
	}
	if d.checked.IsZero() || time.Since(d.checked) >= time.Second {
		keys, err := s.store.List(root + "/mail/")
		if err != nil {
			return "", err
		}
		for _, key := range keys {
			if found := jmapBlobDescriptorID(key); found != "" {
				// Stable selection for duplicate uploads after a cold start.
				if d.paths[found] == "" {
					d.paths[found] = key
				}
			}
		}
		d.checked = time.Now()
	}
	if key := d.paths[string(id)]; key != "" {
		return key, nil
	}
	return root + "/.jmap/blobs/" + string(id), nil // Read-only legacy fallback.
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
	root, _ := jmapRoot(acct)
	directory := s.directory(root)
	directory.mu.Lock()
	part, cached := directory.parts[id]
	directory.mu.Unlock()
	if cached {
		r, err := s.openPart(ctx, acct, part)
		return r, part.Size, err
	}
	r, size, err := openObject(ctx, s.store, key)
	if errors.Is(err, fs.ErrNotExist) {
		data, e := s.store.Get(key + ".part")
		if errors.Is(e, fs.ErrNotExist) {
			return nil, 0, jblob.ErrNotFound
		}
		if e != nil {
			return nil, 0, e
		}
		var part jmapPartManifest
		if e = json.Unmarshal(data, &part); e != nil {
			return nil, 0, e
		}
		if part.Source == id || part.Size < 0 || part.Size > maxAttachmentSize {
			return nil, 0, fmt.Errorf("invalid attachment reference")
		}
		r, e = s.openPart(ctx, acct, part)
		return r, part.Size, e
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

// MIME metadata is an import-time storage record, not a read-through cache.
func (s jmapBlobs) GetMessageMetadata(ctx context.Context, acct, source jmap.Id) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := s.key(acct, source)
	if err != nil {
		return nil, err
	}
	metadataKey := key + ".mime.json"
	if jmapBlobDescriptorID(key) == "" {
		root, _ := jmapRoot(acct)
		metadataKey = root + "/mail/legacy-" + string(source) + "/message.mime.json"
	}
	data, err := s.store.Get(metadataKey)
	if errors.Is(err, fs.ErrNotExist) && metadataKey != key+".mime.json" {
		data, err = s.store.Get(key + ".mime.json")
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}
func (s jmapBlobs) PutMessageMetadata(ctx context.Context, acct, source jmap.Id, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := s.key(acct, source)
	if err != nil {
		return err
	}
	if _, err = jmail.MessagePartIDs(data); err != nil {
		return err
	}
	total, err := jmail.MessageAttachmentBytes(data)
	if err != nil {
		return err
	}
	if total > maxAttachmentSize {
		return fmt.Errorf("attachments exceed 4 GiB total")
	}
	metadataKey := key + ".mime.json"
	if jmapBlobDescriptorID(key) == "" {
		root, _ := jmapRoot(acct)
		metadataKey = root + "/mail/legacy-" + string(source) + "/message.mime.json"
	}
	err = s.store.Create(metadataKey, data)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}
func (s jmapBlobs) CreateMessagePart(ctx context.Context, acct, source jmap.Id, partID, filename string) (jmail.PartWriter, error) {
	root, err := jmapRoot(acct)
	if err != nil {
		return nil, err
	}
	physical, _ := ctx.Value(mimeParentContext{}).(string)
	if physical == "" {
		key, err := s.key(acct, source)
		if err != nil {
			return nil, err
		}
		data, err := s.store.Get(key)
		if err != nil {
			return nil, err
		}
		if ref, e := decodeObjectReference(key, data); e == nil {
			physical = ref.Key[:strings.LastIndex(ref.Key, "/")]
		}
		if physical == "" {
			physical = strings.TrimSuffix(namedBlobKey(root, blobName{MIME: true}, data), "/message.eml")
		}
	}
	// A part-number directory prevents duplicate filenames from overwriting.
	physical += "/attachments/" + objectNameSegment(partID, "part") + "/" + objectNameSegment(filename, "body.bin")
	w := &jmapBlobWriter{ctx: ctx, store: s, account: acct, physicalKey: physical, externalDigest: true}
	return &jmapIngestPart{writer: w, source: source}, nil
}

type jmapIngestPart struct {
	writer *jmapBlobWriter
	source jmap.Id
}

func (p *jmapIngestPart) Write(data []byte) (int, error) {
	if p.writer.size+int64(len(data)) > maxAttachmentSize {
		return 0, fmt.Errorf("attachment exceeds 4 GiB")
	}
	return p.writer.Write(data)
}
func (p *jmapIngestPart) Commit(id jmap.Id, size uint64) error {
	if size != uint64(p.writer.size) {
		return fmt.Errorf("attachment size mismatch")
	}
	p.writer.partID = id
	if _, err := p.writer.Commit(); err != nil {
		return err
	}
	return nil
}
func (p *jmapIngestPart) Abort() error { return p.writer.Abort() }

type jmapPartOrigin struct {
	Source jmap.Id `json:"source"`
}

// Publication in the JMAP upload registry follows parent-email authorization.
// The attachment content was already committed during import.
type jmapStoredPart struct{ id jmap.Id }

func (p jmapStoredPart) Write([]byte) (int, error) { return 0, fmt.Errorf("immutable stored part") }
func (p jmapStoredPart) ID() jmap.Id               { return p.id }
func (p jmapStoredPart) Commit() (jmap.Id, error)  { return p.id, nil }
func (p jmapStoredPart) Abort() error              { return nil }
func (a *jmapAccount) authorizeStoredPart(ctx context.Context, source, id jmap.Id, uploader string) (bool, error) {
	referenced, err := a.db.BlobReferenced(ctx, a.id, source)
	if err != nil || !referenced {
		return false, err
	}
	data, err := a.blobs.GetMessageMetadata(ctx, a.id, source)
	if err != nil || data == nil {
		return false, err
	}
	ids, err := jmail.MessagePartIDs(data)
	if err != nil {
		return false, err
	}
	if !slices.Contains(ids, id) {
		return false, nil
	}
	r, _, err := a.blobs.Open(ctx, a.id, id)
	if err != nil {
		return false, err
	}
	r.Close()
	_, err = a.db.FinalizeBlobUpload(ctx, a.id, jmapStoredPart{id: id}, uploader, time.Now())
	return err == nil, err
}

// Blob IDs belong to the JMAP library; physical objects use human-readable names.
type blobNameContext struct{}
type blobName struct {
	Subject, Filename string
	MIME              bool
}

// Escape each segment independently: slashes, percent signs and dot segments
// must remain literal names, never change the account or directory boundary.
func objectNameSegment(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	var segment strings.Builder
	for _, r := range value {
		escaped := url.PathEscape(string(r))
		if segment.Len()+len(escaped) > 360 {
			break
		}
		segment.WriteString(escaped)
	}
	value = segment.String()
	if value == "." {
		return "%2E"
	}
	if value == ".." {
		return "%2E%2E"
	}
	return value
}

func namedBlobKey(root string, name blobName, prefix []byte) string {
	if name.MIME {
		// Only inspect bounded headers, never read or reconstruct the MIME body.
		end, separator := bytes.Index(prefix, []byte("\r\n\r\n")), 4
		if end < 0 {
			end, separator = bytes.Index(prefix, []byte("\n\n")), 2
		}
		if end >= 0 && end < 64<<10 {
			if msg, err := mail.ReadMessage(bytes.NewReader(prefix[:end+separator])); err == nil {
				name.Subject = msg.Header.Get("Subject")
				if decoded, err := new(mime.WordDecoder).DecodeHeader(name.Subject); err == nil {
					name.Subject = decoded
				}
			}
		}
		if name.Filename == "" {
			name.Filename = "message.eml"
		}
	}
	// A nanosecond UTC timestamp is independent of the untrusted Date header.
	// Conditional S3 writes reject a collision instead of overwriting a message.
	id := objectNameSegment(name.Subject, "untitled") + "_" + time.Now().UTC().Format("20060102T150405.000000000Z")
	return root + "/mail/" + id + "/" + objectNameSegment(name.Filename, "attachment.bin")
}

func (s jmapBlobs) Create(ctx context.Context, acct jmap.Id) (jblob.BlobWriter, error) {
	if _, err := jmapRoot(acct); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &jmapBlobWriter{ctx: ctx, store: s, account: acct, digest: sha256.New()}, nil
}

func (w *jmapBlobWriter) startUpload(extra []byte) error {
	if w.writer != nil {
		return nil
	}
	root, err := jmapRoot(w.account)
	if err != nil {
		return err
	}
	name, ok := w.ctx.Value(blobNameContext{}).(blobName)
	if !ok {
		name.MIME = true
	}
	prefix := extra
	if w.probe != nil {
		prefix = w.probe.Bytes()
	}
	physicalKey := w.physicalKey
	if physicalKey == "" {
		physicalKey = namedBlobKey(root, name, prefix)
	}
	w.physicalKey = physicalKey
	w.writer, err = newObjectUpload(w.ctx, w.store.store, physicalKey)
	return err
}

type jmapBlobWriter struct {
	physicalKey    string
	partID         jmap.Id
	externalDigest bool
	parallelDigest bool // A separate ordered consumer owns digest until receive completes.
	ctx            context.Context
	store          jmapBlobs
	account        jmap.Id
	writer         objectUpload
	digest         hash.Hash
	probe          *bytes.Buffer
	gzip           *gzip.Writer
	writeErr       error
	metadata       map[string]string
	size           int64
	finished       bool
}

func (w *jmapBlobWriter) probeBuffers() *WaitPool[*bytes.Buffer] {
	if w.externalDigest {
		return partProbePool
	}
	return mailProbePool
}
func (w *jmapBlobWriter) compressionWriters() *WaitPool[*gzip.Writer] {
	if w.externalDigest {
		return partGzipWriters
	}
	return gzipWriters
}

func (w *jmapBlobWriter) Write(data []byte) (int, error) {
	if w.finished {
		return 0, fs.ErrClosed
	}
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.size+int64(len(data)) > maxMailSize {
		return 0, fmt.Errorf("MIME object exceeds 6 GiB")
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	var n int
	var err error
	threshold := int64(gzipThreshold)
	if w.externalDigest {
		threshold = 1 << 20
	}
	if w.gzip == nil && w.size+int64(len(data)) <= threshold {
		if w.probe == nil {
			w.probe, err = w.probeBuffers().Get(w.ctx)
			if err != nil {
				return 0, err
			}
		}
		n, err = w.probe.Write(data)
	} else {
		if w.gzip == nil {
			if err = w.startUpload(data); err != nil {
				return 0, err
			}
			w.gzip, err = w.compressionWriters().Get(w.ctx)
			if err != nil {
				return 0, err
			}
			w.gzip.Reset(w.writer)
			if w.probe != nil {
				_, err = w.gzip.Write(w.probe.Bytes())
				w.probeBuffers().Put(w.probe)
				w.probe = nil
			}
			if err != nil {
				w.writeErr = err
				return 0, err
			}
		}
		n, err = w.gzip.Write(data)
	}
	w.writeErr = err
	if !w.externalDigest && !w.parallelDigest {
		w.digest.Write(data[:n])
	}
	w.size += int64(n)
	return n, err
}
func (w *jmapBlobWriter) ID() jmap.Id {
	id := w.partID
	if !w.externalDigest {
		id = jmap.Id("G" + base64.RawURLEncoding.EncodeToString(w.digest.Sum(nil)))
	}
	if w.physicalKey == "" {
		root, _ := jmapRoot(w.account)
		name, ok := w.ctx.Value(blobNameContext{}).(blobName)
		if !ok {
			name.MIME = true
		}
		var prefix []byte
		if w.probe != nil {
			prefix = w.probe.Bytes()
		}
		w.physicalKey = namedBlobKey(root, name, prefix)
	}

	return id
}

func (w *jmapBlobWriter) Commit() (jmap.Id, error) {
	if w.finished {
		return "", fs.ErrClosed
	}
	id := w.ID()
	if !jmapBlobIDRE.MatchString(string(id)) {
		return "", jblob.ErrNotFound
	}
	var err error
	if w.writeErr != nil {
		return "", w.writeErr
	}
	if err = w.startUpload(nil); err != nil {
		return "", err
	}
	if w.gzip != nil {
		err = w.gzip.Close()
		w.compressionWriters().Put(w.gzip)
		w.gzip = nil
		w.metadata = map[string]string{"fma-encoding": "gzip", "fma-size": strconv.FormatInt(w.size, 10)}
	} else {
		if w.probe != nil {
			_, err = w.writer.Write(w.probe.Bytes())
			w.probeBuffers().Put(w.probe)
			w.probe = nil
		}
	}
	if err != nil {
		w.writeErr = err
		return "", err
	}
	// Publish the descriptor beside its immutable bytes, not in a user index.
	key := w.physicalKey + ".blob-" + string(id) + ".json"
	if err = w.writer.Commit(key, w.metadata); err != nil {
		return "", err
	}
	root, _ := jmapRoot(w.account)
	w.store.remember(root, id, key)
	w.finished = true
	return id, nil
}
func (w *jmapBlobWriter) Abort() error {
	w.finished = true
	if w.probe != nil {
		w.probeBuffers().Put(w.probe)
		w.probe = nil
	}
	if w.gzip != nil {
		w.compressionWriters().Put(w.gzip)
		w.gzip = nil
	}
	if w.writer != nil {
		return w.writer.Abort()
	}
	return nil
}

type jmapAccount struct {
	root  string
	id    jmap.Id
	db    *jdb.DB
	proc  *jruntime.Processor
	blobs jmapBlobs
	queue *jsubmit.Queue
}

func newJMAPAccount(root string, store objectStore) (*jmapAccount, error) {
	var be jbackend.Backend
	if _, staged := store.(*jmapBootstrapStore); staged {
		be = &jmapLegacyBackend{key: root + "/.jmap/state.json", store: store}
	} else {
		be = &jmapBackend{key: root + "/.jmap/state.json", store: store}
	}
	a := &jmapAccount{root: root, id: jmapAccountID(root), blobs: jmapBlobs{store: store}, proc: jruntime.NewProcessor()}
	a.db = jdb.New(be, jlease.NewStoreLease(be, jlease.StoreLeaseConfig{}))
	core := jmapCore()
	for _, err := range []error{
		jmail.RegisterMailbox(a.proc, jmail.MailboxConfig{DB: a.db, Core: core}),
		jmail.RegisterThread(a.proc, jmail.ThreadConfig{DB: a.db, Core: core}),
		jmail.RegisterEmail(a.proc, jmail.EmailConfig{DB: a.db, Store: a.blobs, Core: core, AccountCapability: jmapMailCapability(), Searcher: jsearch.New(a.blobs, jsearch.DefaultConfig()), MessageIDDomain: accountDomain(root), InternalProperties: map[string]jdescriptor.Property{"fmaUIDs": {Kind: jdescriptor.KindObject, Default: json.RawMessage(`{}`)}}}),
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
	if err != nil {
		return nil, err
	}
	if owned, ok := be.(*jmapBackend); ok {
		state, e := owned.owner()
		if e != nil {
			return nil, e
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		state.rebuild = func(data map[string][]byte) error {
			for encoded := range data {
				raw, e := hex.DecodeString(encoded)
				if e != nil {
					return e
				}
				if jmapTransientKey(raw) {
					delete(data, encoded)
				}
			}
			var records []struct {
				key   string
				value []byte
			}
			for key, value := range data {
				records = append(records, struct {
					key   string
					value []byte
				}{key, value})
			}
			for _, record := range records {
				raw, _ := hex.DecodeString(record.key)
				parts := jmapKeySegments(raw)
				if len(parts) == 3 && parts[1] == "u" {
					data[hex.EncodeToString(jmapIndexKey(string(a.id), "p", parts[2]))] = []byte{}
				}
				if len(parts) != 4 || parts[1] != "o" {
					continue
				}
				data[hex.EncodeToString(jmapIndexKey("!tag", "exists", string(a.id)))] = []byte{}
				// Tags and collection hints are supersets; the worker/sweeper verifies
				// authoritative records before acting, then drops drained hints.
				if parts[2] == "EmailSubmission" {
					data[hex.EncodeToString(jmapIndexKey("!tag", "mail:submission-queued", string(a.id)))] = []byte{}
				}
				batch, e := jmapObjectIndexes(a.db.Type(parts[2]), a.id, jmap.Id(parts[3]), record.value)
				if e != nil {
					return e
				}
				for _, op := range batch.Ops {
					key := hex.EncodeToString(op.Key)
					switch op.Kind {
					case jbackend.OpSet:
						data[key] = append([]byte{}, op.Value...)
					case jbackend.OpDelete:
						delete(data, key)
					}
				}
			}
			return nil
		}
		if e = state.refresh(context.Background(), false); e != nil {
			return nil, e
		}
		if e = state.rebuild(state.data); e != nil {
			return nil, e
		}
		state.ordered = nil
	}
	return a, nil
}
func jmapCore() jmap.CoreCapabilities {
	c := jruntime.DefaultCoreCapabilities()
	c.MaxSizeUpload = jmapMaxUpload
	return c
}
func (a *jmapAccount) identity() *jauth.Identity {
	return &jauth.Identity{Username: a.root, Primary: a.id, Accounts: map[jmap.Id]jauth.Access{a.id: {Name: accountAddress(a.root), Personal: true}}}
}
func (a *jmapAccount) CanSend(ctx context.Context, id jmap.Id) (bool, string) {
	if err := ctx.Err(); err != nil {
		return false, err.Error()
	}
	if id != a.id {
		return false, "account not accessible"
	}
	root, err := resolveIdentity(accountAddress(a.root))
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
	root, err := resolveIdentity(accountAddress(user))
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
		size := jvalue[int64](email, "size")
		result = append(result, &memory.Message{Uid: uid, Date: jvalue[time.Time](email, "receivedAt"), Size: uint32(min(size, int64(^uint32(0)))), Flags: jmapFlags(jvalue[map[string]bool](email, "keywords"))})
		ids[uid] = id
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Uid < result[j].Uid })
	return result, ids, nil
}
func jmapForKey(key string) (*jmapAccount, error) {
	return openJMAPAccount(context.Background(), accountFromKey(key))
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

type jmapAccountReady struct {
	done    chan struct{}
	account *jmapAccount
	err     error
}

var jmapAccounts = struct {
	sync.Mutex
	items map[jmapOwnerCacheKey]*jmapAccountReady
}{items: map[jmapOwnerCacheKey]*jmapAccountReady{}}

func openJMAPAccount(ctx context.Context, root string) (*jmapAccount, error) {
	if !validAccountRoot(root) {
		return nil, jauth.ErrUnauthenticated
	}
	key := jmapOwnerCacheKey{objects, root}
	jmapAccounts.Lock()
	entry := jmapAccounts.items[key]
	if entry != nil {
		jmapAccounts.Unlock()
		select {
		case <-entry.done:
			return entry.account, entry.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	entry = &jmapAccountReady{done: make(chan struct{})}
	jmapAccounts.items[key] = entry
	jmapAccounts.Unlock()
	entry.account, entry.err = openJMAPAccountUncached(ctx, root)
	jmapAccounts.Lock()
	if entry.err != nil {
		delete(jmapAccounts.items, key)
	}
	close(entry.done)
	jmapAccounts.Unlock()
	return entry.account, entry.err
}
func flushJMAPOwners(ctx context.Context) error {
	jmapOwnerStates.Lock()
	var states []*jmapOwnerState
	for key, s := range jmapOwnerStates.states {
		if key.store == objects {
			states = append(states, s)
		}
	}
	jmapOwnerStates.Unlock()
	var errs []error
	for _, s := range states {
		s.mu.Lock()
		if s.loaded && s.finalized != s.head.Transaction {
			if e := s.refresh(ctx, true); e != nil {
				errs = append(errs, e)
			} else if e = s.flush(ctx); e != nil {
				errs = append(errs, e)
			}
		}
		s.mu.Unlock()
	}
	return errors.Join(errs...)
}
func serveJMAPOwnerFlush(ctx context.Context) error {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			if err := flushJMAPOwners(ctx); err != nil {
				logger.Error("account metadata flush failed", "error", err)
			}
		}
	}
}
func openJMAPAccountUncached(ctx context.Context, root string) (*jmapAccount, error) {
	if !validAccountRoot(root) {
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
	if _, err = a.call(ctx, "Identity/set", jmapSetRequest[jmapIdentityCreate]{Create: map[string]jmapIdentityCreate{"fma": {Name: root, Email: accountAddress(root)}}}); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	// Publish owner records directly: bootstrap's temporary indexes never
	// leave process memory, even for the first version of the account file.
	var data map[string][]byte
	if err = json.Unmarshal(staged.data, &data); err != nil {
		return nil, err
	}
	state := &jmapOwnerState{key: key, store: objects, data: data, locations: map[string]string{}, head: jmapOwnerFile{Format: 1, User: map[string][]byte{}}}
	delta := map[string]*[]byte{}
	for key, value := range data {
		raw, e := hex.DecodeString(key)
		if e != nil {
			return nil, e
		}
		if jmapTransientKey(raw) {
			continue
		}
		v := bytes.Clone(value)
		delta[key] = &v
	}
	if err = state.persist(ctx, delta); err != nil && !errors.Is(err, fs.ErrExist) {
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
	if strings.HasPrefix(r.URL.Path, "/upload/") {
		typ, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		_, disposition, _ := mime.ParseMediaType(r.Header.Get("Content-Disposition"))
		name := blobName{Subject: r.Header.Get("Subject"), Filename: disposition["filename"], MIME: typ == "message/rfc822" || typ == "message/global"}
		r = r.WithContext(context.WithValue(r.Context(), blobNameContext{}, name))
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
		base = "https://mail." + accountDomain(identity.RootID)
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
		user, routeErr := deliveryUser(address)
		if routeErr != nil {
			return results, routeErr
		}
		if user != "" {
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
				sendErr = sendRemote(ctx, &outboundJob{User: sender.root, From: env.MailFrom, Blob: ref}, address)
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

// Delimiter listing discovers accounts without listing every message and
// without a persistent queue/account index. Small test stores use flat listing.
// Delimiter listing bounds discovery to one directory level on real S3.
func storePrefixes(store objectStore, prefix string) ([]string, error) {
	if guard, ok := store.(*guardedStore); ok {
		guard.mu.RLock()
		defer guard.mu.RUnlock()
		if guard.closed {
			return nil, net.ErrClosed
		}
		return storePrefixes(guard.base, prefix)
	}
	found := map[string]bool{}
	if bucket, ok := store.(*s3Bucket); ok {
		ctx, cancel := bucket.requestContext()
		defer cancel()
		pages := s3.NewListObjectsV2Paginator(bucket.client, &s3.ListObjectsV2Input{Bucket: &bucket.bucket, Prefix: &prefix, Delimiter: aws.String("/")})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, item := range page.CommonPrefixes {
				found[aws.ToString(item.Prefix)] = true
			}
		}
	} else {
		keys, err := store.List(prefix)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			rest := strings.TrimPrefix(key, prefix)
			part, _, ok := strings.Cut(rest, "/")
			if ok {
				found[prefix+part+"/"] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(found)), nil
}
func jmapAccountRoots(store objectStore, namespaces ...string) ([]string, error) {
	if len(namespaces) == 0 {
		prefixes, err := storePrefixes(store, "")
		if err != nil {
			return nil, err
		}
		for _, prefix := range prefixes {
			domain := strings.TrimSuffix(prefix, "/")
			if validMailDomain(domain) {
				namespaces = append(namespaces, domain)
			}
		}
	}
	var roots []string
	for _, namespace := range namespaces {
		prefixes, err := storePrefixes(store, namespace+"/")
		if err != nil {
			return nil, err
		}
		for _, prefix := range prefixes {
			root := strings.TrimSuffix(prefix, "/")
			if validAccountRoot(root) {
				roots = append(roots, root)
			}
		}
	}
	return roots, nil
}

func scanJMAPTasks(ctx context.Context, lease *bucketLease, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	roots, err := jmapAccountRoots(objects, lease.namespace)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, root := range roots {
		if accountDomain(root) != lease.namespace {
			continue
		}
		if ctx.Err() != nil || !lease.valid(time.Now()) || count >= limit {
			break
		}
		// Discover committed account files, including submissions after a restart.
		if _, err := objects.Get(root + "/.jmap/state.json"); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return count, err
		}
		a, err := openJMAPAccount(ctx, root)
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
	key, err := a.blobs.key(a.id, id)
	if err != nil {
		return err
	}
	if parent, _, ok := strings.Cut(key, "/attachments/"); ok {
		directory := a.blobs.directory(a.root)
		directory.mu.Lock()
		var source jmap.Id
		for candidate, location := range directory.paths {
			if path.Dir(location) == parent {
				source = jmap.Id(candidate)
				break
			}
		}
		directory.mu.Unlock()
		if source != "" {
			if found, e := a.authorizeStoredPart(ctx, source, id, uploader); e != nil || found {
				return e
			}
		}
	}
	if data, e := a.blobs.store.Get(key + ".origin.json"); e == nil {
		var origin jmapPartOrigin
		if e = json.Unmarshal(data, &origin); e != nil {
			return e
		}
		if found, e := a.authorizeStoredPart(ctx, origin.Source, id, uploader); e != nil || found {
			return e
		}
	} else if !errors.Is(e, fs.ErrNotExist) {
		return e
	}
	emails, err := a.all(ctx, "Email")
	if err != nil {
		return err
	}
	for _, email := range emails {
		source := jvalue[jmap.Id](email, "blobId")
		if metadata, e := a.blobs.GetMessageMetadata(ctx, a.id, source); e != nil {
			return e
		} else if metadata != nil {
			if found, e := a.authorizeStoredPart(ctx, source, id, uploader); e != nil || found {
				return e
			}
			continue
		}
		r, _, err := a.blobs.Open(ctx, a.id, jvalue[jmap.Id](email, "blobId"))
		if err != nil {
			return err
		}
		msg, err := mail.ReadMessage(r)
		if err != nil {
			r.Close()
			continue
		}
		part, found, err := a.findPart(ctx, textproto.MIMEHeader(msg.Header), msg.Body, id, jvalue[jmap.Id](email, "blobId"), nil)
		r.Close()
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		_, err = a.db.FinalizeBlobUpload(ctx, a.id, jmapPartWriter{store: a.blobs, account: a.id, id: id, part: part}, uploader, time.Now())
		return err
	}
	return jblob.ErrNotFound
}

// Decoded attachments are virtual views of the immutable original MIME blob.
// Store a small locator instead of a second multi-gigabyte copy.
type jmapPartManifest struct {
	Source jmap.Id `json:"source"`
	Path   []int   `json:"path"`
	Size   int64   `json:"size"`
}
type jmapPartWriter struct {
	store       jmapBlobs
	account, id jmap.Id
	part        jmapPartManifest
}

func (w jmapPartWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("immutable part reference") }
func (w jmapPartWriter) ID() jmap.Id               { return w.id }
func (w jmapPartWriter) Abort() error              { return nil }
func (w jmapPartWriter) Commit() (jmap.Id, error) {
	root, err := jmapRoot(w.account)
	if err != nil {
		return "", err
	}
	if !jmapBlobIDRE.MatchString(string(w.id)) || w.part.Source == w.id || w.part.Size < 0 || w.part.Size > maxAttachmentSize {
		return "", fmt.Errorf("invalid attachment locator")
	}
	directory := w.store.directory(root)
	directory.mu.Lock()
	part := w.part
	part.Path = slices.Clone(part.Path)
	directory.parts[w.id] = part
	directory.mu.Unlock()
	return w.id, nil
}
func decodeMIME(r io.Reader, h textproto.MIMEHeader) io.Reader {
	switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
	case "base64":
		return &mimeBase64Reader{source: r}
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	}
	return r
}

// Base64 MIME input is compacted in a reusable buffer. Decode writes directly
// into a sufficiently large caller buffer; small callers use the input buffer
// in place, retaining at most one block and a three-byte quartet tail.
var mimeBase64Buffers = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

type mimeBase64Reader struct {
	source       io.Reader
	buffer       *[]byte
	pending      []byte
	tail         [3]byte
	ntail        int
	readErr, err error
}

func (r *mimeBase64Reader) release() {
	if r.buffer != nil {
		mimeBase64Buffers.Put(r.buffer)
		r.buffer = nil
	}
}

func compactMIMENewlines(p []byte) int {
	written := 0
	for pos := 0; pos < len(p); {
		i := bytes.IndexByte(p[pos:], '\r')
		j := bytes.IndexByte(p[pos:], '\n')
		if i < 0 || j >= 0 && j < i {
			i = j
		}
		if i < 0 {
			i = len(p) - pos
		}
		if written != pos {
			copy(p[written:], p[pos:pos+i])
		}
		written += i
		pos += i
		if pos < len(p) {
			pos++
		}
	}
	return written
}

func (r *mimeBase64Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) != 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	if r.err != nil {
		r.release()
		return 0, r.err
	}
	if r.buffer == nil {
		r.buffer = mimeBase64Buffers.Get().(*[]byte)
	}
	buf := *r.buffer
	nbuf := copy(buf, r.tail[:r.ntail])
	r.ntail = 0
	for empty := 0; nbuf < 4 && r.readErr == nil; {
		limit := min(max(len(p)/3*4, 4), len(buf))
		n, err := r.source.Read(buf[nbuf:limit])
		r.readErr = err
		nbuf += compactMIMENewlines(buf[nbuf : nbuf+n])
		if n == 0 && err == nil {
			empty++
			if empty == 100 {
				r.readErr = io.ErrNoProgress
			}
		} else {
			empty = 0
		}
	}
	if nbuf < 4 {
		r.err = r.readErr
		if r.err == io.EOF && nbuf > 0 {
			r.err = io.ErrUnexpectedEOF
		}
		r.release()
		return 0, r.err
	}
	nr := nbuf / 4 * 4
	r.ntail = copy(r.tail[:], buf[nr:nbuf])
	if len(p) >= nr/4*3 {
		n, err := base64.StdEncoding.Decode(p, buf[:nr])
		r.err = err
		if err != nil {
			r.release()
		}
		return n, err
	}
	// Decoding contracts the data; unread encoded bytes stay ahead of writes.
	n, err := base64.StdEncoding.Decode(buf, buf[:nr])
	r.err = err
	r.pending = buf[:n]
	n = copy(p, r.pending)
	r.pending = r.pending[n:]
	if len(r.pending) == 0 && err != nil {
		r.release()
		return n, err
	}
	return n, nil
}

func (s jmapBlobs) openPart(ctx context.Context, acct jmap.Id, part jmapPartManifest) (io.ReadCloser, error) {
	key, err := s.key(acct, part.Source)
	if err != nil {
		return nil, err
	}
	// A locator can only refer to a physical original, never another locator.
	source, _, err := openObject(ctx, s.store, key)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (io.ReadCloser, error) { source.Close(); return nil, err }
	msg, err := mail.ReadMessage(source)
	if err != nil {
		return fail(err)
	}
	h := textproto.MIMEHeader(msg.Header)
	body := msg.Body
	if len(part.Path) > 30 {
		return fail(fmt.Errorf("MIME nesting exceeds 30"))
	}
	for _, index := range part.Path {
		body = decodeMIME(body, h)
		if index == 0 {
			msg, err = mail.ReadMessage(body)
			if err != nil {
				return fail(err)
			}
			h = textproto.MIMEHeader(msg.Header)
			body = msg.Body
			continue
		}
		_, params, err := mime.ParseMediaType(h.Get("Content-Type"))
		if err != nil {
			return fail(err)
		}
		parts := multipart.NewReader(body, params["boundary"])
		for n := 1; n <= index; n++ {
			p, err := parts.NextRawPart()
			if err != nil {
				return fail(err)
			}
			h = p.Header
			body = p
		}
	}
	return &jmapDownloadReader{ctx: ctx, reader: decodeMIME(body, h), Closer: source}, nil
}

// Fill the caller's buffer before returning decoded attachment bytes. Base64
// produces short reads (at most 768 bytes), which otherwise become millions of
// small HTTP writes. No attachment-sized allocation or read-ahead is needed.
type jmapDownloadReader struct {
	io.Closer
	ctx    context.Context
	reader io.Reader
	err    error
}

// Keep HTTP's ReaderFrom fast path from choosing a smaller output buffer. Both
// wrappers deliberately expose only Read/Write, preventing io.Copy recursion.
// The shared pool bounds memory across concurrent transfers.
func (r *jmapDownloadReader) WriteTo(w io.Writer) (int64, error) {
	return copyStream(r.ctx, struct{ io.Writer }{w}, struct{ io.Reader }{r})
}

func (r *jmapDownloadReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	p = p[:min(len(p), 128<<10)]
	for empty := 0; n < len(p); {
		if r.err != nil {
			return n, r.err
		}
		if r.err = r.ctx.Err(); r.err != nil {
			return n, r.err
		}
		var k int
		k, r.err = r.reader.Read(p[n:])
		n += k
		if k == 0 && r.err == nil {
			empty++
			if empty == 100 {
				r.err = io.ErrNoProgress
			}
		} else {
			empty = 0
		}
	}
	return n, r.err
}

func (a *jmapAccount) findPart(ctx context.Context, h textproto.MIMEHeader, r io.Reader, wanted, source jmap.Id, indices []int) (jmapPartManifest, bool, error) {
	var empty jmapPartManifest
	if err := ctx.Err(); err != nil {
		return empty, false, err
	}
	if len(indices) > 30 {
		return empty, false, fmt.Errorf("MIME nesting exceeds 30")
	}
	r = decodeMIME(r, h)
	typ, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		typ = "text/plain"
	}
	if strings.HasPrefix(typ, "multipart/") {
		parts := multipart.NewReader(r, params["boundary"])
		for index := 1; ; index++ {
			p, err := parts.NextRawPart()
			if err == io.EOF {
				return empty, false, nil
			}
			if err != nil {
				return empty, false, err
			}
			part, found, err := a.findPart(ctx, p.Header, p, wanted, source, append(slices.Clone(indices), index))
			p.Close()
			if found || err != nil {
				return part, found, err
			}
		}
	}
	digest := sha256.New()
	size, err := copyStream(ctx, digest, io.LimitReader(r, maxAttachmentSize+1))
	if err != nil {
		return empty, false, err
	}
	if size > maxAttachmentSize {
		return empty, false, fmt.Errorf("attachment exceeds 4 GiB")
	}
	part := jmapPartManifest{Source: source, Path: indices, Size: size}
	if jmap.Id("G"+base64.RawURLEncoding.EncodeToString(digest.Sum(nil))) == wanted {
		return part, true, nil
	}
	if typ == "message/rfc822" || typ == "message/global" {
		reader, err := a.blobs.openPart(ctx, a.id, part)
		if err != nil {
			return empty, false, err
		}
		defer reader.Close()
		msg, err := mail.ReadMessage(reader)
		if err == nil {
			return a.findPart(ctx, textproto.MIMEHeader(msg.Header), msg.Body, wanted, source, append(slices.Clone(indices), 0))
		}
	}
	return empty, false, nil
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
// Uploads assemble legal 8 MiB S3 parts from reusable 1 MiB blocks without
// concatenating them. The global pool admits at most 1 GiB of active blocks.
const s3PartSize = 8 << 20
const s3BlockSize = 1 << 20
const s3BufferLimit = 1 << 30
const gzipThreshold = 10 << 20
const (
	maxAttachmentSize = 4 << 30
	// Base64 with CRLF every 76 characters expands 4 GiB to about 5.48 GiB.
	maxMailSize         = 6 << 30
	jmapMaxUpload int64 = 4 << 30 // HTTP uploads; internally composed MIME may be larger.
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
	if jmapBlobDescriptorID(key) != "" {
		ref, err := decodeObjectReference(key, data)
		if err != nil {
			return nil, 0, err
		}
		data, err = store.Get(ref.Key)
		if err != nil {
			return nil, 0, err
		}
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}
func newObjectUpload(ctx context.Context, store objectStore, prefix string) (objectUpload, error) {
	if s, ok := store.(streamingStore); ok {
		return s.NewUpload(ctx, prefix)
	}
	return &bufferedObjectUpload{ctx: ctx, store: store, physical: prefix}, nil // Small in-memory test stores only.
}

type bufferedObjectUpload struct {
	physical string
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
	data := w.Bytes()
	if jmapBlobDescriptorID(key) != "" {
		if err := w.store.Create(w.physical, data); err != nil {
			return err
		}
		data = jraw(namedObjectReference{Key: w.physical})
	}
	err := w.store.Create(key, data)
	if errors.Is(err, fs.ErrExist) {
		err = nil
	}
	w.Reset()
	return err
}
func (w *bufferedObjectUpload) Abort() error { w.Reset(); return nil }

// An immutable blob descriptor records the physical bytes and their encoding.
// New descriptors live beside those bytes; ID lookup tables exist only in memory.
type namedObjectReference struct {
	Key      string            `json:"key"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func decodeObjectReference(key string, data []byte) (namedObjectReference, error) {
	var ref namedObjectReference
	if len(data) > 8192 {
		return ref, fmt.Errorf("object reference too large")
	}
	if err := json.Unmarshal(data, &ref); err != nil {
		return ref, err
	}
	root := accountFromKey(key)
	if !validAccountRoot(root) || !strings.HasPrefix(ref.Key, root+"/mail/") || path.Clean(ref.Key) != ref.Key || len(ref.Key) > 1024 {
		return ref, fmt.Errorf("object reference escapes account")
	}
	if ref.Metadata["fma-reference"] != "" {
		return ref, fmt.Errorf("nested object reference")
	}
	return ref, nil
}
func (b *s3Bucket) putObjectReference(ctx context.Context, key string, ref namedObjectReference, create bool) error {
	data, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	if _, err = decodeObjectReference(key, data); err != nil {
		return err
	}
	input := &s3.PutObjectInput{Bucket: &b.bucket, Key: &key, Body: bytes.NewReader(data), Metadata: map[string]string{"fma-reference": "1"}}
	if create {
		input.IfNoneMatch = aws.String("*")
	}
	_, err = b.client.PutObject(ctx, input)
	err = objectError(key, err)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}
func (b *s3Bucket) OpenStream(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	result, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &b.bucket, Key: &key})
	if err != nil {
		return nil, 0, objectError(key, err)
	}
	if result.Metadata["fma-reference"] == "1" {
		data, err := io.ReadAll(io.LimitReader(result.Body, 8193))
		result.Body.Close()
		if err != nil {
			return nil, 0, err
		}
		ref, err := decodeObjectReference(key, data)
		if err != nil {
			return nil, 0, err
		}
		result, err = b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &b.bucket, Key: &ref.Key})
		if err != nil {
			return nil, 0, objectError(ref.Key, err)
		}
		result.Metadata = ref.Metadata
	}
	if result.Metadata["fma-encoding"] == "gzip" {
		size, err := strconv.ParseInt(result.Metadata["fma-size"], 10, 64)
		if err != nil || size < 0 || size > maxMailSize {
			result.Body.Close()
			return nil, 0, fmt.Errorf("invalid uncompressed blob size")
		}
		reader, err := gzip.NewReader(bufio.NewReaderSize(result.Body, 128<<10))
		if err != nil {
			result.Body.Close()
			return nil, 0, err
		}
		return &gzipObjectReader{Reader: reader, source: result.Body}, size, nil
	}
	return result.Body, aws.ToInt64(result.ContentLength), nil
}

type gzipObjectReader struct {
	Reader io.ReadCloser
	source io.ReadCloser
}

func (r *gzipObjectReader) Read(p []byte) (int, error) { return r.Reader.Read(p) }
func (r *gzipObjectReader) Close() error               { return errors.Join(r.Reader.Close(), r.source.Close()) }
func (b *s3Bucket) CopyStream(ctx context.Context, src, dst string) error {
	head, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b.bucket, Key: &src})
	if err != nil {
		return objectError(src, err)
	}
	if head.Metadata["fma-reference"] != "1" {
		return b.copyStream(ctx, src, dst, nil)
	}
	result, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &b.bucket, Key: &src})
	if err != nil {
		return objectError(src, err)
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, 8193))
	result.Body.Close()
	if err != nil {
		return err
	}
	ref, err := decodeObjectReference(src, data)
	if err != nil {
		return err
	}
	srcRoot := accountFromKey(src)
	dstRoot := accountFromKey(dst)
	if !validAccountRoot(dstRoot) {
		return fmt.Errorf("invalid destination account")
	}
	if srcRoot != dstRoot {
		// Re-delivery to the same content-addressed account index is idempotent.
		if path.Base(src) == path.Base(dst) && (jmapBlobIDRE.MatchString(path.Base(dst)) || jmapBlobDescriptorID(dst) != "") {
			_, existing := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b.bucket, Key: &dst})
			if existing == nil {
				return nil
			}
			if !errors.Is(objectError(dst, existing), fs.ErrNotExist) {
				return objectError(dst, existing)
			}
		}
		target := dstRoot + strings.TrimPrefix(ref.Key, srcRoot)
		// Reserve the destination, then let S3 copy the stored bytes. The
		// application never downloads or re-uploads the object body.
		upload, err := b.NewUpload(ctx, target)
		if err != nil {
			return err
		}
		defer upload.Abort()
		if err = b.copyStream(ctx, ref.Key, target, ref.Metadata); err != nil {
			return err
		}
		upload.(*s3StreamUpload).physicalComplete = true
		return upload.Commit(dst, ref.Metadata)
	}
	return b.putObjectReference(ctx, dst, ref, false)
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
	// Legacy objects above 5 GiB need multipart copy.
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
	completion := &s3.CompleteMultipartUploadInput{Bucket: &b.bucket, Key: &dst, UploadId: upload.UploadId, MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts}}
	_, err = b.client.CompleteMultipartUpload(ctx, completion)
	complete = err == nil
	return objectError(dst, err)
}
func (b *s3Bucket) NewUpload(ctx context.Context, key string) (objectUpload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	// Reserve the final name atomically before accepting body bytes. Some S3
	// compatible servers ignore conditions on CompleteMultipartUpload; a second
	// fma writer must fail at the conditional PUT, not overwrite the first.
	reserved, err := b.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &b.bucket, Key: &key, Body: bytes.NewReader(nonce[:]), IfNoneMatch: aws.String("*"), Metadata: map[string]string{"fma-pending": "1"}})
	if err != nil {
		return nil, objectError(key, err)
	}
	if reserved.ETag == nil {
		return nil, fmt.Errorf("S3 reservation returned no ETag")
	}
	ctx, cancel := context.WithCancel(ctx)
	return &s3StreamUpload{bucket: b, ctx: ctx, cancel: cancel, key: key, reservation: reserved.ETag}, nil
}

// An S3 request body owns these blocks until the request (including retries)
// finishes. Seek permits SDK retries without joining the blocks or rereading S3.
type s3PartBuffer struct {
	blocks [s3PartSize / s3BlockSize][]byte
	size   int
	offset int64
}

func (p *s3PartBuffer) Len() int { return p.size }
func (p *s3PartBuffer) Write(data []byte) (int, error) {
	if len(data) > s3PartSize-p.size {
		return 0, io.ErrShortWrite
	}
	written := 0
	for len(data) > 0 {
		i, off := p.size/s3BlockSize, p.size%s3BlockSize
		if p.blocks[i] == nil {
			p.blocks[i] = make([]byte, s3BlockSize)
		}
		n := copy(p.blocks[i][off:], data)
		data = data[n:]
		p.size += n
		written += n
	}
	return written, nil
}
func (p *s3PartBuffer) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if p.offset >= int64(p.size) {
		return 0, io.EOF
	}
	n := min(len(data), p.size-int(p.offset))
	read := 0
	for read < n {
		i, off := int(p.offset)/s3BlockSize, int(p.offset)%s3BlockSize
		k := copy(data[read:n], p.blocks[i][off:])
		read += k
		p.offset += int64(k)
	}
	return n, nil
}
func (p *s3PartBuffer) Seek(offset int64, whence int) (int64, error) {
	next := offset
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		next += p.offset
	case io.SeekEnd:
		next += int64(p.size)
	default:
		return 0, fmt.Errorf("invalid S3 seek origin")
	}
	if next < 0 {
		return 0, fmt.Errorf("negative S3 seek offset")
	}
	p.offset = next
	return next, nil
}

type s3StreamUpload struct {
	reservation      *string
	physicalComplete bool
	cancel           context.CancelFunc
	workers          sync.WaitGroup
	partMu           sync.Mutex
	partErr          error
	bucket           *s3Bucket
	ctx              context.Context
	key              string
	uploadID         *string
	parts            []s3types.CompletedPart
	buffer           *s3PartBuffer
	slots            chan struct{}
	finished         bool
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
		if w.buffer == nil {
			if w.slots == nil {
				w.slots = make(chan struct{}, 4)
			}
			select {
			case w.slots <- struct{}{}:
			case <-w.ctx.Done():
				return written, w.ctx.Err()
			}
			var err error
			w.buffer, err = s3BufferPool.Get(w.ctx)
			if err != nil {
				<-w.slots
				return written, err
			}
		}
		n := min(s3PartSize-w.buffer.Len(), len(data))
		w.buffer.Write(data[:n])
		data = data[n:]
		written += n
		if w.buffer.Len() == s3PartSize {
			if err := w.flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (w *s3StreamUpload) releaseBuffer(buffer *s3PartBuffer) {
	s3BufferPool.Put(buffer)
	<-w.slots
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
	w.partMu.Lock()
	index := len(w.parts)
	w.parts = append(w.parts, s3types.CompletedPart{})
	w.partMu.Unlock()
	buffer := w.buffer
	w.buffer = nil // The worker owns this buffer until UploadPart returns.
	w.workers.Add(1)
	go func() {
		defer w.workers.Done()
		defer w.releaseBuffer(buffer)
		number := int32(index + 1)
		result, err := b.client.UploadPart(w.ctx, &s3.UploadPartInput{Bucket: &b.bucket, Key: &w.key, UploadId: w.uploadID, PartNumber: &number, Body: buffer, ContentLength: aws.Int64(int64(buffer.Len()))})
		w.partMu.Lock()
		defer w.partMu.Unlock()
		if err != nil {
			if w.partErr == nil {
				w.partErr = objectError(w.key, err)
			}
			w.cancel()
		} else {
			w.parts[index] = s3types.CompletedPart{ETag: result.ETag, PartNumber: &number}
		}
	}()
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
	completeStart := time.Now()
	if !w.physicalComplete {
		if w.uploadID == nil {
			var body io.ReadSeeker = bytes.NewReader(nil)
			if w.buffer != nil {
				body = w.buffer
				w.buffer.Seek(0, io.SeekStart)
			}
			_, err := b.client.PutObject(w.ctx, &s3.PutObjectInput{Bucket: &b.bucket, Key: &w.key, Body: body, IfMatch: w.reservation, Metadata: metadata})
			if err != nil {
				return objectError(w.key, err)
			}
		} else {
			if w.buffer != nil && w.buffer.Len() > 0 {
				if err := w.flush(); err != nil {
					return err
				}
			}
			w.workers.Wait()
			if w.partErr != nil {
				return w.partErr
			}
			_, err := b.client.CompleteMultipartUpload(w.ctx, &s3.CompleteMultipartUploadInput{Bucket: &b.bucket, Key: &w.key, UploadId: w.uploadID, IfMatch: w.reservation, MultipartUpload: &s3types.CompletedMultipartUpload{Parts: w.parts}})
			if err != nil {
				return objectError(w.key, err)
			}
		}
		w.physicalComplete = true
	}
	completeElapsed := time.Since(completeStart)
	indexStart := time.Now()
	if err := b.putObjectReference(w.ctx, key, namedObjectReference{Key: w.key, Metadata: metadata}, true); err != nil {
		return err
	}
	logger.Debug("S3 stream commit", "parts", len(w.parts), "complete_seconds", completeElapsed.Seconds(), "index_seconds", time.Since(indexStart).Seconds(), "copy_seconds", 0)
	w.finished = true
	w.cancel()
	w.workers.Wait()
	if w.buffer != nil {
		w.releaseBuffer(w.buffer)
		w.buffer = nil
	}
	return nil
}

func (w *s3StreamUpload) Abort() error {
	if w.finished {
		return nil
	}
	w.finished = true
	w.cancel()
	w.workers.Wait()
	if w.buffer != nil {
		w.releaseBuffer(w.buffer)
		w.buffer = nil
	}
	// Once completed, an index PUT may have succeeded despite a lost response.
	if w.physicalComplete {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), 30*time.Second)
	defer cancel()
	b := w.bucket
	var err error
	if w.uploadID != nil {
		_, err = b.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &b.bucket, Key: &w.key, UploadId: w.uploadID})
		err = objectError(w.key, err)
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	// No index was published; remove our pending reservation. A failed multipart
	// abort retains the name to prevent a second writer racing late completion.
	if err != nil {
		return err
	}
	_, err = b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b.bucket, Key: &w.key})
	return objectError(w.key, err)
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

// Workers pull from one bounded queue: an idle worker takes the next task,
// so a slow transfer cannot pin subsequent tasks to that worker's private queue.
type streamTask struct {
	ctx  context.Context
	run  func(context.Context) error
	done chan error
}
type streamTaskPool struct {
	jobs   chan streamTask
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.RWMutex
	closed bool
	wg     sync.WaitGroup
}

func newStreamTaskPool(workers int) *streamTaskPool {
	if workers < 1 {
		panic("stream task pool requires a worker")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &streamTaskPool{jobs: make(chan streamTask, workers), ctx: ctx, cancel: cancel}
	p.wg.Add(workers)
	for range workers {
		go func() {
			defer p.wg.Done()
			for job := range p.jobs {
				ctx, cancel := context.WithCancel(job.ctx)
				stop := context.AfterFunc(p.ctx, cancel)
				if p.ctx.Err() != nil {
					cancel()
				}
				err := ctx.Err()
				if err == nil {
					err = job.run(ctx)
				}
				stop()
				cancel()
				job.done <- err
			}
		}()
	}
	return p
}
func (p *streamTaskPool) Submit(ctx context.Context, run func(context.Context) error) (<-chan error, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, net.ErrClosed
	}
	job := streamTask{ctx: ctx, run: run, done: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ctx.Done():
		return nil, net.ErrClosed
	case p.jobs <- job:
		return job.done, nil
	}
}
func (p *streamTaskPool) Close() {
	p.cancel()
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.jobs)
	}
	p.mu.Unlock()
	p.wg.Wait()
}

var streamTasks = newStreamTaskPool(defaultStreamWorkers)

// Each immutable block is returned only after the parser and every writer finish.
// All consumers share three 1 MiB allocations; no stream-sized copies are retained.
type mimeStreamBlock struct {
	data []byte
	refs atomic.Int32
	free chan []byte
}

func (b *mimeStreamBlock) release() {
	if b.refs.Add(-1) == 0 {
		b.free <- b.data[:cap(b.data)]
	}
}

type mimeStreamBlocks struct {
	ctx     context.Context
	ready   chan *mimeStreamBlock
	free    chan []byte
	current []byte
	owned   *mimeStreamBlock
	err     error // written before ready closes, read after receiving the closed channel
}

func newMIMEStreamBlocks() *mimeStreamBlocks {
	return &mimeStreamBlocks{ctx: context.Background(), ready: make(chan *mimeStreamBlock, 3), free: make(chan []byte, 3)}
}
func (s *mimeStreamBlocks) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.current) == 0 {
		var block *mimeStreamBlock
		var ok bool
		select {
		case block, ok = <-s.ready:
		case <-s.ctx.Done():
			return 0, s.ctx.Err()
		}
		if !ok {
			if s.err != nil {
				return 0, s.err
			}
			return 0, io.EOF
		}
		s.current, s.owned = block.data, block
	}
	n := copy(p, s.current)
	if n == len(s.current) {
		s.owned.release()
		s.current, s.owned = nil, nil
	} else {
		s.current = s.current[n:]
	}
	return n, nil
}
func (s *mimeStreamBlocks) receive(ctx context.Context, r io.Reader, writers ...io.Writer) (size int64, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	queues := make([]chan *mimeStreamBlock, len(writers))
	results := make(chan error, len(writers))
	for i, w := range writers {
		q := make(chan *mimeStreamBlock, 3)
		queues[i] = q
		go func() {
			var failure error
			for block := range q {
				if failure == nil {
					failure = ctx.Err()
					if failure == nil {
						n, e := w.Write(block.data)
						failure = e
						if failure == nil && n != len(block.data) {
							failure = io.ErrShortWrite
						}
					}
					if failure != nil {
						cancel()
					}
				}
				block.release()
			}
			results <- failure
		}()
	}
	defer func() {
		for _, q := range queues {
			close(q)
		}
		for range writers {
			if e := <-results; e != nil && (err == nil || errors.Is(err, context.Canceled)) {
				err = e
			}
		}
		s.err = err
		close(s.ready)
	}()
	for range 3 {
		s.free <- make([]byte, 1<<20)
	}
	empty := 0
	for {
		var b []byte
		select {
		case b = <-s.free:
		case <-ctx.Done():
			return size, ctx.Err()
		}
		n, e := r.Read(b)
		if n > 0 {
			empty = 0
			if size+int64(n) > maxMailSize {
				return size, &smtp.SMTPError{Code: 552, Message: "message too large"}
			}
			block := &mimeStreamBlock{data: b[:n], free: s.free}
			// The producer reference prevents reuse while dispatch is still in progress.
			block.refs.Store(1)
			for _, q := range queues {
				block.refs.Add(1)
				select {
				case q <- block:
				case <-ctx.Done():
					block.release()
					block.release()
					return size, ctx.Err()
				}
			}
			block.refs.Add(1)
			select {
			case s.ready <- block:
			case <-ctx.Done():
				block.release()
				block.release()
				return size, ctx.Err()
			}
			block.release()
			size += int64(n)
		} else {
			s.free <- b
			empty++
			if empty >= 100 {
				return size, io.ErrNoProgress
			}
		}
		if e == io.EOF {
			return size, nil
		}
		if e != nil {
			return size, e
		}
	}
}

// The raw MIME uploader and MIME parser consume the same incoming bytes.
// A bounded block queue applies backpressure and transfers buffer ownership.
type mimeParentContext struct{}
type mimePipelineStore struct {
	jmapBlobs
	metadata []byte
}

func (s *mimePipelineStore) PutMessageMetadata(_ context.Context, _, _ jmap.Id, data []byte) error {
	s.metadata = bytes.Clone(data)
	return nil
}
func (s *mimePipelineStore) publish(ctx context.Context, account, id jmap.Id) error {

	return s.jmapBlobs.PutMessageMetadata(ctx, account, id, s.metadata)
}
func storeMailStream(ctx context.Context, root string, r io.Reader) (*jmapBlobRef, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	readStart := time.Now()
	input := bufio.NewReaderSize(r, 128<<10)
	prefix, err := input.Peek(64 << 10)
	if err != nil && err != io.EOF {
		return nil, err
	}
	physical := namedBlobKey(root, blobName{MIME: true}, prefix)
	ctx = context.WithValue(ctx, mimeParentContext{}, physical[:strings.LastIndex(physical, "/")])
	store := jmapBlobs{store: objects}
	account := jmapAccountID(root)
	writer := &jmapBlobWriter{ctx: ctx, store: store, account: account, digest: sha256.New(), parallelDigest: true, physicalKey: physical}
	defer writer.Abort()
	pipeline := &mimePipelineStore{jmapBlobs: store}
	stopPoolCancel := context.AfterFunc(streamTasks.ctx, cancel)
	defer stopPoolCancel()
	blocks := newMIMEStreamBlocks()
	started := make(chan struct{})
	received := make(chan struct{})
	done, err := streamTasks.Submit(ctx, func(taskCtx context.Context) error {
		blocks.ctx = taskCtx // Only the worker reads this field.
		close(started)
		defer func() { <-received }() // Hold admission until both auxiliary consumers exit.
		err := jmail.IngestMessage(taskCtx, pipeline, account, "", blocks)
		if err == nil {
			_, err = io.Copy(io.Discard, blocks)
		}
		if err != nil {
			cancel()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	// Helpers belong to an admitted ingestion, never to jobs waiting in the pool.
	// They do not submit dependent tasks to the pool, so even one worker is safe.
	select {
	case <-started:
	case <-ctx.Done():
		close(received)
		<-done
		return nil, ctx.Err()
	}
	size, readErr := blocks.receive(ctx, input, writer, writer.digest)
	close(received)
	if readErr != nil {
		cancel()
	}
	parseErr := <-done
	if parseErr != nil {
		return nil, parseErr
	}
	if readErr != nil {
		return nil, readErr
	}
	if size > maxMailSize {
		return nil, &smtp.SMTPError{Code: 552, Message: "message too large"}
	}
	readElapsed := time.Since(readStart)
	commitStart := time.Now()
	id, err := writer.Commit()
	if err != nil {
		return nil, err
	}
	if err = pipeline.publish(ctx, account, id); err != nil {
		return nil, err
	}
	logger.Debug("mail stream stored", "bytes", size, "read_seconds", readElapsed.Seconds(), "commit_seconds", time.Since(commitStart).Seconds())
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
	root, _ := jmapRoot(w.account)
	sourceRoot, _ := jmapRoot(w.source.AccountID)
	if stream, ok := w.store.store.(streamingStore); ok && jmapBlobDescriptorID(src) != "" {
		dst := root + strings.TrimPrefix(src, sourceRoot)
		err = stream.CopyStream(w.ctx, src, dst)
		if err == nil {
			w.store.remember(root, id, dst)
		}
	} else {
		reader, _, e := w.store.Open(w.ctx, w.source.AccountID, id)
		if e != nil {
			return "", e
		}
		defer reader.Close()
		writer, e := w.store.Create(w.ctx, w.account)
		if e != nil {
			return "", e
		}
		defer writer.Abort()
		if _, e = copyStream(w.ctx, writer, reader); e != nil {
			return "", e
		}
		_, err = writer.Commit()
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
	started := time.Now()
	defer func() {
		logger.Debug("mail stream delivery", "recipients", len(users), "sent", sent, "elapsed_seconds", time.Since(started).Seconds())
	}()
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
		if err = writeJSON(path.Join(queueDirectory(owner), job.ID+".json"), job); err != nil {
			return err
		}
	}
	logger.Info("SMTP DATA stored as S3 stream", "user", s.user, "bytes", ref.Size, "local", len(s.recipients), "remote", len(s.remote))
	return nil
}

// WaitPool follows WireGuard's count/condition-variable design, with typed
// values and cancellable waits. Copyright (C) 2017-2025 WireGuard LLC.
// Adapted under the MIT license; see LICENSE and README acknowledgements.
// A borrowed value belongs exclusively to its caller until Put.
type WaitPool[T any] struct {
	pool       sync.Pool
	cond       *sync.Cond
	mu         sync.Mutex
	count, max int
	reset      func(T)
}

func newWaitPool[T any](max int, create func() T, reset func(T)) *WaitPool[T] {
	if max <= 0 {
		panic("WaitPool requires a positive capacity")
	}
	p := &WaitPool[T]{max: max, reset: reset}
	p.pool.New = func() any { return create() }
	p.cond = sync.NewCond(&p.mu)
	return p
}
func (p *WaitPool[T]) Get(ctx context.Context) (T, error) {
	var zero T
	p.mu.Lock()
	if p.count >= p.max {
		stop := context.AfterFunc(ctx, func() { p.mu.Lock(); p.cond.Broadcast(); p.mu.Unlock() })
		defer stop()
		for p.count >= p.max && ctx.Err() == nil {
			p.cond.Wait()
		}
	}
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return zero, err
	}
	p.count++
	p.mu.Unlock()
	return p.pool.Get().(T), nil
}
func (p *WaitPool[T]) Put(value T) {
	if p.reset != nil {
		p.reset(value)
	}
	p.pool.Put(value)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.count <= 0 {
		panic("WaitPool Put without Get")
	}
	p.count--
	p.cond.Signal()
}
func (p *WaitPool[T]) outstanding() int { p.mu.Lock(); defer p.mu.Unlock(); return p.count }
func newBufferPool(count, size int) *WaitPool[*bytes.Buffer] {
	return newWaitPool(count, func() *bytes.Buffer { return bytes.NewBuffer(make([]byte, 0, size)) }, func(b *bytes.Buffer) {
		if b.Cap() != size {
			*b = *bytes.NewBuffer(make([]byte, 0, size))
		} else {
			b.Reset()
		}
	})
}

var (
	mailProbePool   = newBufferPool(4, gzipThreshold)
	partProbePool   = newBufferPool(defaultStreamWorkers, 1<<20)
	partGzipWriters = newWaitPool(defaultStreamWorkers, func() *gzip.Writer { w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return w }, func(w *gzip.Writer) { w.Reset(io.Discard) })
	s3BufferPool    = newWaitPool(s3BufferLimit/s3PartSize, func() *s3PartBuffer { return new(s3PartBuffer) }, func(p *s3PartBuffer) { p.size, p.offset = 0, 0 })
	copyBufferPool  = newBufferPool(32, 128<<10)
	gzipWriters     = newWaitPool(4, func() *gzip.Writer { w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return w }, func(w *gzip.Writer) { w.Reset(io.Discard) })
)

func copyStream(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer, err := copyBufferPool.Get(ctx)
	if err != nil {
		return 0, err
	}
	defer copyBufferPool.Put(buffer)
	return io.CopyBuffer(dst, src, buffer.AvailableBuffer()[:buffer.Cap()])
}

// Protocol snapshots contain metadata only. Blob references remain stable even
// when another session changes mailbox membership while a download is active.
func mailBlobRefs(ctx context.Context, key string) (map[uint32]jmapBlobRef, error) {
	if !useJMAP {
		return nil, nil
	}
	a, err := jmapForKey(key)
	if err != nil {
		return nil, err
	}
	box, err := a.mailbox(ctx, key)
	if err != nil {
		return nil, err
	}
	emails, err := a.all(ctx, "Email")
	if err != nil {
		return nil, err
	}
	refs := make(map[uint32]jmapBlobRef)
	for _, email := range emails {
		if !jvalue[map[jmap.Id]bool](email, "mailboxIds")[box] {
			continue
		}
		uid := jvalue[map[jmap.Id]uint32](email, "fmaUIDs")[box]
		if uid != 0 {
			refs[uid] = jmapBlobRef{AccountID: a.id, BlobID: jvalue[jmap.Id](email, "blobId"), Size: jvalue[int64](email, "size")}
		}
	}
	return refs, nil
}
func storedMessageSize(m *memory.Message, refs map[uint32]jmapBlobRef) int64 {
	if ref, ok := refs[m.Uid]; ok {
		return ref.Size
	}
	return int64(len(m.Body))
}
func openStoredMessage(ctx context.Context, m *memory.Message, refs map[uint32]jmapBlobRef) (io.ReadCloser, int64, error) {
	if ref, ok := refs[m.Uid]; ok {
		return (jmapBlobs{store: objects}).Open(ctx, ref.AccountID, ref.BlobID)
	}
	if useJMAP {
		return nil, 0, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(m.Body)), int64(len(m.Body)), nil
}

// IMAP v2 parses APPEND as a LiteralReader; the v1 parser allocates the
// entire literal before calling the backend. Reuse mailbox mutations while
// keeping payload IO out of metadata snapshots and response buffers.
type imapStreamSession struct {
	ctx      context.Context
	cancel   context.CancelFunc
	user     *imapUser
	box      *inbox
	refs     map[uint32]jmapBlobRef
	readOnly bool
}

func (s *imapStreamSession) Close() error { s.cancel(); return nil }
func (s *imapStreamSession) Login(name, password string) error {
	account, err := authenticateAccount(name, password)
	if err != nil {
		return imapserver.ErrAuthFailed
	}
	s.user = &imapUser{name: account.RootID, loginID: account.LoginID}
	return nil
}
func imapFlags2(flags []string) []imap2.Flag {
	out := make([]imap2.Flag, len(flags))
	for i, f := range flags {
		out[i] = imap2.Flag(f)
	}
	return out
}
func imapFlags1(flags []imap2.Flag) []string {
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = string(f)
	}
	return out
}
func (s *imapStreamSession) Select(name string, opts *imap2.SelectOptions) (*imap2.SelectData, error) {
	mailbox, err := s.user.GetMailbox(name)
	if err != nil {
		return nil, err
	}
	s.box = mailbox.(*inbox)
	s.readOnly = opts.ReadOnly
	s.refs, err = mailBlobRefs(s.ctx, s.box.key())
	if err != nil {
		return nil, err
	}
	status, err := s.box.Status([]imap.StatusItem{imap.StatusMessages})
	if err != nil {
		return nil, err
	}
	data := &imap2.SelectData{Flags: imapFlags2(status.Flags), PermanentFlags: imapFlags2(status.PermanentFlags), NumMessages: uint32(len(s.box.Messages)), UIDNext: imap2.UID(s.box.next), UIDValidity: s.box.meta.Validity}
	for i, m := range s.box.Messages {
		if !slices.Contains(m.Flags, imap.SeenFlag) {
			data.FirstUnseenSeqNum = uint32(i + 1)
			break
		}
	}
	return data, nil
}
func (s *imapStreamSession) Create(name string, _ *imap2.CreateOptions) error {
	return s.user.CreateMailbox(name)
}
func (s *imapStreamSession) Delete(name string) error { return s.user.DeleteMailbox(name) }
func (s *imapStreamSession) Rename(old, name string, _ *imap2.RenameOptions) error {
	return s.user.RenameMailbox(old, name)
}
func (s *imapStreamSession) Subscribe(name string) error   { return s.subscribe(name, true) }
func (s *imapStreamSession) Unsubscribe(name string) error { return s.subscribe(name, false) }
func (s *imapStreamSession) subscribe(name string, value bool) error {
	box, err := s.user.GetMailbox(name)
	if err != nil {
		return err
	}
	return box.SetSubscribed(value)
}
func (s *imapStreamSession) List(w *imapserver.ListWriter, ref string, patterns []string, opts *imap2.ListOptions) error {
	boxes, err := s.user.ListMailboxes(opts.SelectSubscribed)
	if err != nil {
		return err
	}
	for _, box := range boxes {
		matched := false
		for _, pattern := range patterns {
			if imapserver.MatchList(box.Name(), '/', ref, pattern) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		info, err := box.Info()
		if err != nil {
			return err
		}
		data := &imap2.ListData{Mailbox: box.Name(), Delim: '/'}
		for _, attr := range info.Attributes {
			data.Attrs = append(data.Attrs, imap2.MailboxAttr(attr))
		}
		if opts.ReturnStatus != nil {
			data.Status, err = s.Status(box.Name(), opts.ReturnStatus)
			if err != nil {
				return err
			}
		}
		if err = w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}
func (s *imapStreamSession) Status(name string, opts *imap2.StatusOptions) (*imap2.StatusData, error) {
	mailbox, err := s.user.GetMailbox(name)
	if err != nil {
		return nil, err
	}
	b := mailbox.(*inbox)
	status, err := b.Status([]imap.StatusItem{imap.StatusMessages})
	if err != nil {
		return nil, err
	}
	data := &imap2.StatusData{Mailbox: b.name, UIDNext: imap2.UID(b.next), UIDValidity: b.meta.Validity}
	count := uint32(len(b.Messages))
	recent := uint32(0)
	if opts.NumMessages {
		data.NumMessages = &count
	}
	if opts.NumRecent {
		data.NumRecent = &recent
	}
	if opts.NumUnseen {
		data.NumUnseen = &status.Unseen
	}
	return data, nil
}
func (s *imapStreamSession) AppendLimit() uint32 { return ^uint32(0) }
func (s *imapStreamSession) Append(name string, r imap2.LiteralReader, opts *imap2.AppendOptions) (*imap2.AppendData, error) {
	meta, err := getFolder(s.user.name, canonicalFolder(name))
	if err != nil {
		return nil, err
	}
	ref, err := storeMailStream(s.ctx, s.user.name, r)
	if err != nil {
		return nil, err
	}
	a, err := jmapForKey(meta.Key)
	if err != nil {
		return nil, err
	}
	box, err := a.mailbox(s.ctx, meta.Key)
	if err != nil {
		return nil, err
	}
	id, err := a.importStoredMail(s.ctx, box, ref, imapFlags1(opts.Flags), opts.Time, false)
	if err != nil {
		return nil, err
	}
	uid, err := a.uid(s.ctx, box, id)
	return &imap2.AppendData{UID: imap2.UID(uid), UIDValidity: meta.Validity}, err
}
func (s *imapStreamSession) Unselect() error { s.box = nil; s.refs = nil; return nil }
func (s *imapStreamSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.box == nil {
		return nil
	}
	old := s.box.Messages
	oldNext := s.box.next
	if err := s.box.Poll(); err != nil {
		return err
	}
	now := s.box.Messages
	byUID := map[uint32]*memory.Message{}
	for _, m := range now {
		byUID[m.Uid] = m
	}
	var removed []uint32
	for i := len(old) - 1; i >= 0; i-- {
		if byUID[old[i].Uid] == nil {
			removed = append(removed, uint32(i+1))
		}
	}
	if len(removed) > 0 && !allowExpunge {
		s.box.Messages = old
		s.box.next = oldNext
		return nil
	}
	refs, err := mailBlobRefs(s.ctx, s.box.key())
	if err != nil {
		return err
	}
	s.refs = refs
	for _, seq := range removed {
		if err = w.WriteExpunge(seq); err != nil {
			return err
		}
	}
	if len(now) != len(old) {
		if err = w.WriteNumMessages(uint32(len(now))); err != nil {
			return err
		}
	}
	oldFlags := map[uint32][]string{}
	for _, m := range old {
		oldFlags[m.Uid] = m.Flags
	}
	for i, m := range now {
		if previous, ok := oldFlags[m.Uid]; ok && !slices.Equal(previous, m.Flags) {
			if err = w.WriteMessageFlags(uint32(i+1), imap2.UID(m.Uid), imapFlags2(m.Flags)); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *imapStreamSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	for {
		changed := imapChangeSignal()
		if err := s.Poll(w, true); err != nil {
			return err
		}
		select {
		case <-stop:
			return nil
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-changed:
		}
	}
}
func (s *imapStreamSession) Expunge(w *imapserver.ExpungeWriter, uids *imap2.UIDSet) error {
	if s.readOnly {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	for i := len(s.box.Messages) - 1; i >= 0; i-- {
		m := s.box.Messages[i]
		if !slices.Contains(m.Flags, imap.DeletedFlag) || uids != nil && !uids.Contains(imap2.UID(m.Uid)) {
			continue
		}
		if err := removeMessage(s.box.key(), m.Uid); err != nil {
			return err
		}
		s.box.Messages = slices.Delete(s.box.Messages, i, i+1)
		delete(s.refs, m.Uid)
		if err := w.WriteExpunge(uint32(i + 1)); err != nil {
			return err
		}
	}
	return nil
}
func imapSet1(set imap2.NumSet) (bool, *imap.SeqSet, error) {
	_, uid := set.(imap2.UIDSet)
	parsed, err := imap.ParseSeqSet(set.String())
	return uid, parsed, err
}
func (s *imapStreamSession) Store(w *imapserver.FetchWriter, set imap2.NumSet, flags *imap2.StoreFlags, _ *imap2.StoreOptions) error {
	if s.readOnly {
		return fmt.Errorf("mailbox is read-only")
	}
	uid, seq, err := imapSet1(set)
	if err != nil {
		return err
	}
	op := imap.SetFlags
	if flags.Op == imap2.StoreFlagsAdd {
		op = imap.AddFlags
	} else if flags.Op == imap2.StoreFlagsDel {
		op = imap.RemoveFlags
	}
	if err = s.box.UpdateMessagesFlags(uid, seq, op, imapFlags1(flags.Flags)); err != nil {
		return err
	}
	if !flags.Silent {
		return s.Fetch(w, set, &imap2.FetchOptions{Flags: true, UID: uid})
	}
	return nil
}
func (s *imapStreamSession) Copy(set imap2.NumSet, dest string) (*imap2.CopyData, error) {
	uid, seq, err := imapSet1(set)
	if err != nil {
		return nil, err
	}
	meta, err := getFolder(s.user.name, canonicalFolder(dest))
	if err != nil {
		return nil, err
	}
	a, err := jmapForKey(meta.Key)
	if err != nil {
		return nil, err
	}
	box, err := a.mailbox(s.ctx, meta.Key)
	if err != nil {
		return nil, err
	}
	data := &imap2.CopyData{UIDValidity: meta.Validity}
	for _, m := range s.box.selected(uid, seq) {
		ref, ok := s.refs[m.Uid]
		if !ok {
			return nil, fs.ErrNotExist
		}
		id, err := a.importStoredMail(s.ctx, box, &ref, m.Flags, m.Date, false)
		if err != nil {
			return nil, err
		}
		n, err := a.uid(s.ctx, box, id)
		if err != nil {
			return nil, err
		}
		data.SourceUIDs.AddNum(imap2.UID(m.Uid))
		data.DestUIDs.AddNum(imap2.UID(n))
	}
	return data, nil
}
func (s *imapStreamSession) Fetch(w *imapserver.FetchWriter, set imap2.NumSet, opts *imap2.FetchOptions) error {
	uid, seq, err := imapSet1(set)
	if err != nil {
		return err
	}
	s.box.resolve(seq, uid)
	for i, m := range s.box.Messages {
		n := uint32(i + 1)
		selected := n
		if uid {
			selected = m.Uid
		}
		if !seq.Contains(selected) {
			continue
		}
		if !s.readOnly {
			for _, section := range opts.BodySection {
				if !section.Peek {
					single := new(imap.SeqSet)
					single.AddNum(m.Uid)
					if err = s.box.UpdateMessagesFlags(true, single, imap.AddFlags, []string{imap.SeenFlag}); err != nil {
						return err
					}
					break
				}
			}
		}
		response := w.CreateMessage(n)
		if opts.UID || uid {
			response.WriteUID(imap2.UID(m.Uid))
		}
		if opts.Flags {
			response.WriteFlags(imapFlags2(m.Flags))
		}
		if opts.InternalDate {
			response.WriteInternalDate(m.Date)
		}
		if opts.RFC822Size {
			response.WriteRFC822Size(storedMessageSize(m, s.refs))
		}
		if opts.Envelope || opts.BodyStructure != nil {
			r, _, err := openStoredMessage(s.ctx, m, s.refs)
			if err != nil {
				return err
			}
			br := bufio.NewReader(r)
			h, err := msgtext.ReadHeader(br)
			if err != nil {
				r.Close()
				return err
			}
			if opts.Envelope {
				response.WriteEnvelope(imapserver.ExtractEnvelope(h))
			}
			if opts.BodyStructure != nil {
				bs, err := backendutil.FetchBodyStructure(h, br, opts.BodyStructure.Extended)
				if err != nil {
					r.Close()
					return err
				}
				response.WriteBodyStructure(imapBodyStructure2(bs))
			}
			r.Close()
		}
		for _, section := range opts.BodySection {
			open := func() (io.ReadCloser, int64, error) { return openIMAPSection(s.ctx, m, s.refs, section) }
			r, size, err := open()
			if err != nil {
				return err
			}
			if size < 0 {
				size, err = copyStream(s.ctx, io.Discard, r)
				r.Close()
				if err != nil {
					return err
				}
				r, _, err = open()
				if err != nil {
					return err
				}
			}
			dst := response.WriteBodySection(section, size)
			_, err = copyStream(s.ctx, dst, r)
			r.Close()
			closeErr := dst.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		if err = response.Close(); err != nil {
			return err
		}
	}
	return nil
}
func imapBodyStructure2(bs *imap.BodyStructure) imap2.BodyStructure {
	disp := &imap2.BodyStructureDisposition{Value: bs.Disposition, Params: bs.DispositionParams}
	if bs.Disposition == "" {
		disp = nil
	}
	if bs.MIMEType == "multipart" {
		out := &imap2.BodyStructureMultiPart{Subtype: bs.MIMESubType, Extended: &imap2.BodyStructureMultiPartExt{Params: bs.Params, Disposition: disp, Language: bs.Language, Location: strings.Join(bs.Location, " ")}}
		for _, part := range bs.Parts {
			out.Children = append(out.Children, imapBodyStructure2(part))
		}
		return out
	}
	out := &imap2.BodyStructureSinglePart{Type: bs.MIMEType, Subtype: bs.MIMESubType, Params: bs.Params, ID: bs.Id, Description: bs.Description, Encoding: bs.Encoding, Size: bs.Size, Extended: &imap2.BodyStructureSinglePartExt{Disposition: disp, Language: bs.Language, Location: strings.Join(bs.Location, " ")}}
	if bs.MIMEType == "text" {
		out.Text = &imap2.BodyStructureText{NumLines: int64(bs.Lines)}
	}
	if bs.BodyStructure != nil {
		envelope := &imap2.Envelope{}
		if bs.Envelope != nil {
			envelope.Subject = bs.Envelope.Subject
			envelope.Date = bs.Envelope.Date
		}
		out.MessageRFC822 = &imap2.BodyStructureMessageRFC822{Envelope: envelope, BodyStructure: imapBodyStructure2(bs.BodyStructure), NumLines: int64(bs.Lines)}
	}
	return out
}

type sectionReadCloser struct {
	io.Reader
	io.Closer
}

func openIMAPSection(ctx context.Context, m *memory.Message, refs map[uint32]jmapBlobRef, item *imap2.FetchItemBodySection) (io.ReadCloser, int64, error) {
	source, size, err := openStoredMessage(ctx, m, refs)
	if err != nil {
		return nil, 0, err
	}
	var body io.Reader = source
	fail := func(err error) (io.ReadCloser, int64, error) { source.Close(); return nil, 0, err }
	if len(item.Part) > 0 || item.Specifier != "" || len(item.HeaderFields) > 0 || len(item.HeaderFieldsNot) > 0 {
		br := bufio.NewReader(source)
		h, err := msgtext.ReadHeader(br)
		if err != nil {
			return fail(err)
		}
		body = br
		for i, n := range item.Part {
			typ, params, _ := mime.ParseMediaType(h.Get("Content-Type"))
			if typ == "message/rfc822" || typ == "message/global" {
				nested := bufio.NewReader(body)
				h, err = msgtext.ReadHeader(nested)
				if err != nil {
					return fail(err)
				}
				body = nested
				typ, params, _ = mime.ParseMediaType(h.Get("Content-Type"))
			}
			if !strings.HasPrefix(typ, "multipart/") {
				if i == 0 && n == 1 {
					continue
				}
				source.Close()
				return io.NopCloser(strings.NewReader("")), 0, nil
			}
			parts := msgtext.NewMultipartReader(body, params["boundary"])
			for j := 1; j <= n; j++ {
				part, err := parts.NextPart()
				if errors.Is(err, io.EOF) {
					source.Close()
					return io.NopCloser(strings.NewReader("")), 0, nil
				}
				if err != nil {
					return fail(err)
				}
				h = part.Header
				body = part
			}
		}
		if len(item.Part) > 0 && (item.Specifier == imap2.PartSpecifierHeader || item.Specifier == imap2.PartSpecifierText) {
			typ, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
			if typ == "message/rfc822" || typ == "message/global" {
				nested := bufio.NewReader(body)
				h, err = msgtext.ReadHeader(nested)
				if err != nil {
					return fail(err)
				}
				body = nested
			}
		}
		if len(item.HeaderFields) > 0 {
			fields := map[string]bool{}
			for _, k := range item.HeaderFields {
				fields[strings.ToLower(k)] = true
			}
			for f := h.Fields(); f.Next(); {
				if !fields[strings.ToLower(f.Key())] {
					f.Del()
				}
			}
		}
		for _, k := range item.HeaderFieldsNot {
			h.Del(k)
		}
		header := new(bytes.Buffer)
		writeHeader := item.Specifier == imap2.PartSpecifierHeader || item.Specifier == imap2.PartSpecifierMIME || item.Specifier == "" && len(item.Part) == 0
		if writeHeader {
			if err = msgtext.WriteHeader(header, h); err != nil {
				return fail(err)
			}
		}
		if item.Specifier == imap2.PartSpecifierHeader || item.Specifier == imap2.PartSpecifierMIME {
			body = header
			size = int64(header.Len())
		} else {
			body = io.MultiReader(header, body)
			size = -1
		}
	}
	if item.Partial != nil {
		skip := item.Partial.Offset
		n, err := io.CopyN(io.Discard, body, skip)
		if err != nil && !errors.Is(err, io.EOF) {
			return fail(err)
		}
		if n < skip {
			size = 0
			body = strings.NewReader("")
		} else {
			if size >= 0 {
				size = min(max(0, size-skip), item.Partial.Size)
			}
			body = io.LimitReader(body, item.Partial.Size)
		}
	}
	return &sectionReadCloser{Reader: body, Closer: source}, size, nil
}

func (s *imapStreamSession) Search(kind imapserver.NumKind, c *imap2.SearchCriteria, _ *imap2.SearchOptions) (*imap2.SearchData, error) {
	result := &imap2.SearchData{}
	var seqs imap2.SeqSet
	var uids imap2.UIDSet
	for i, m := range s.box.Messages {
		seq := uint32(i + 1)
		ok, err := s.matchMessage(m, seq, c)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		n := seq
		if kind == imapserver.NumKindUID {
			n = m.Uid
			uids.AddNum(imap2.UID(n))
		} else {
			seqs.AddNum(n)
		}
		if result.Count == 0 {
			result.Min = n
		}
		result.Max = n
		result.Count++
	}
	if kind == imapserver.NumKindUID {
		result.All = uids
	} else {
		result.All = seqs
	}
	return result, nil
}
func (s *imapStreamSession) matchMessage(m *memory.Message, seq uint32, c *imap2.SearchCriteria) (bool, error) {
	for _, sets := range []struct {
		uid  bool
		sets []string
	}{{false, imapSetStrings(c.SeqNum)}, {true, imapSetStrings(c.UID)}} {
		for _, text := range sets.sets {
			set, err := imap.ParseSeqSet(text)
			if err != nil {
				return false, err
			}
			s.box.resolve(set, sets.uid)
			n := seq
			if sets.uid {
				n = m.Uid
			}
			if !set.Contains(n) {
				return false, nil
			}
		}
	}
	size := storedMessageSize(m, s.refs)
	if c.Larger > 0 && size <= c.Larger || c.Smaller > 0 && size >= c.Smaller {
		return false, nil
	}
	r, _, err := openStoredMessage(s.ctx, m, s.refs)
	if err != nil {
		return false, err
	}
	entity, err := message.Read(r)
	if entity == nil {
		r.Close()
		return false, err
	}
	r.Close()
	basic := &imap.SearchCriteria{Since: c.Since, Before: c.Before, SentSince: c.SentSince, SentBefore: c.SentBefore, WithFlags: imapFlags1(c.Flag), WithoutFlags: imapFlags1(c.NotFlag), Header: make(textproto.MIMEHeader)}
	if !basic.Since.IsZero() {
		basic.Since = basic.Since.AddDate(0, 0, -1)
	}
	for _, h := range c.Header {
		basic.Header.Add(h.Key, h.Value)
	}
	ok, err := backendutil.Match(entity, seq, m.Uid, m.Date, m.Flags, basic)
	if err != nil || !ok {
		return false, err
	}
	for _, term := range c.Body {
		ok, err := s.bodyContains(m, term)
		if err != nil || !ok {
			return false, err
		}
	}
	for _, term := range c.Text {
		found := false
		for h := entity.Header.Fields(); h.Next(); {
			value, _ := h.Text()
			if strings.Contains(strings.ToLower(h.Key()+": "+value), strings.ToLower(term)) {
				found = true
				break
			}
		}
		if !found {
			ok, err := s.bodyContains(m, term)
			if err != nil || !ok {
				return false, err
			}
		}
	}
	for _, not := range c.Not {
		ok, err := s.matchMessage(m, seq, &not)
		if err != nil || ok {
			return false, err
		}
	}
	for _, pair := range c.Or {
		one, err := s.matchMessage(m, seq, &pair[0])
		if err != nil {
			return false, err
		}
		if !one {
			two, err := s.matchMessage(m, seq, &pair[1])
			if err != nil || !two {
				return false, err
			}
		}
	}
	return true, nil
}
func imapSetStrings[T interface{ String() string }](sets []T) []string {
	out := make([]string, len(sets))
	for i, set := range sets {
		out[i] = set.String()
	}
	return out
}
func (s *imapStreamSession) bodyContains(m *memory.Message, term string) (bool, error) {
	if term == "" {
		return true, nil
	}
	r, _, err := openStoredMessage(s.ctx, m, s.refs)
	if err != nil {
		return false, err
	}
	defer r.Close()
	entity, err := message.Read(r)
	if entity == nil {
		return false, err
	}
	needle := strings.ToLower(term)
	buffer, err := copyBufferPool.Get(s.ctx)
	if err != nil {
		return false, err
	}
	defer copyBufferPool.Put(buffer)
	chunk := buffer.AvailableBuffer()[:buffer.Cap()]
	var tail []byte
	for {
		n, e := entity.Body.Read(chunk)
		if n > 0 {
			data := append(tail, chunk[:n]...)
			if strings.Contains(strings.ToLower(string(data)), needle) {
				return true, nil
			}
			keep := min(len(data), len(needle)*4+4)
			tail = append(tail[:0], data[len(data)-keep:]...)
		}
		if e == io.EOF {
			return false, nil
		}
		if e != nil {
			return false, e
		}
	}
}

var imapChanges = struct {
	sync.Mutex
	signal chan struct{}
}{signal: make(chan struct{})}

func imapChangeSignal() <-chan struct{} {
	imapChanges.Lock()
	defer imapChanges.Unlock()
	return imapChanges.signal
}
func notifyIMAP() {
	imapChanges.Lock()
	close(imapChanges.signal)
	imapChanges.signal = make(chan struct{})
	imapChanges.Unlock()
}
